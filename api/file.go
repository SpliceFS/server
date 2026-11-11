package api

import (
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/SpliceFS/server/internal/wire"
	"github.com/SpliceFS/server/share"
)

// defaultContentType is used for a file whose extension is not known.
const defaultContentType = "application/octet-stream"

// validatorHeaders are the request headers that make the answer depend on the exact
// representation of the file.
var validatorHeaders = []string{"If-Match", "If-None-Match", "If-Range", "If-Modified-Since", "If-Unmodified-Since"}

// file answers GET and HEAD /v1/file/{share}/{name...}.
func (s *server) file(w http.ResponseWriter, r *http.Request, sh Share) {
	name := r.PathValue("name")
	if r.Method == http.MethodHead {
		s.head(w, r, sh, name)
		return
	}
	f, err := sh.Open(name)
	if err != nil {
		s.failLookup(w, r, err, share.ErrNotFound, share.ErrIsDir)
		return
	}
	defer func() { _ = f.Close() }()
	s.get(w, r, f)
}

// head answers HEAD with the headers a GET would carry, plus the head digest.
func (s *server) head(w http.ResponseWriter, r *http.Request, sh Share, name string) {
	digest, info, err := sh.HeadDigest(name)
	if err != nil {
		s.failLookup(w, r, err, share.ErrNotFound, share.ErrIsDir)
		return
	}
	h := w.Header()
	setFileHeaders(h, info)
	h.Set("Content-Length", strconv.FormatInt(info.Size, 10))
	h.Set("Accept-Ranges", "bytes")
	h.Set(wire.HeaderHeadDigest, digest)
	w.WriteHeader(http.StatusOK)
}

// get sends the file f, which is the file that the ETag and the Last-Modified time
// describe, so that the preconditions of the request are checked against it.
func (s *server) get(w http.ResponseWriter, r *http.Request, f File) {
	info := f.Info()
	contentType := setFileHeaders(w.Header(), info)
	switch {
	case multipleRanges(r.Header.Get("Range")):
		// A multipart answer costs the server little and the client a lot to ask for
		// many tiny ranges; the API serves one range per request.
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(info.Size, 10))
		writeError(w, http.StatusRequestedRangeNotSatisfiable, errorMessage(http.StatusRequestedRangeNotSatisfiable))
	case shouldCompress(r, contentType, info.Size):
		s.sendCompressed(w, r, f, info)
	default:
		// The Content-Type is already set, so ServeContent does not sniff the content.
		http.ServeContent(&errorBodyWriter{ResponseWriter: w}, r, "", info.MTime, f)
	}
}

// sendCompressed sends the whole file with gzip. It is a different representation of
// the file, so it gets a different tag: a block request that holds the tag of the plain
// file must never be satisfied by it.
func (s *server) sendCompressed(w http.ResponseWriter, r *http.Request, f File, info share.Info) {
	w.Header().Set("ETag", compressedETag(info.ETag))
	if err := writeGzip(w, f); err != nil {
		s.log.DebugContext(r.Context(), "compressed response ended early",
			"path", r.URL.Path, "error", err)
	}
}

// setFileHeaders sets the headers that describe a file and do not depend on the request,
// and returns the content type it chose.
func setFileHeaders(h http.Header, info share.Info) string {
	contentType := contentTypeOf(info.Name)
	h.Set("Content-Type", contentType)
	h.Set("ETag", info.ETag)
	h.Set("Last-Modified", info.MTime.UTC().Format(http.TimeFormat))
	if compressible(contentType) {
		h.Add("Vary", "Accept-Encoding")
	}
	return contentType
}

// contentTypeOf returns the type for the extension of name. The content is never looked
// at: sniffing would make the type depend on bytes the server does not control.
func contentTypeOf(name string) string {
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return defaultContentType
}

// compressedETag turns a quoted tag into the tag of the gzip representation.
func compressedETag(etag string) string {
	return strings.TrimSuffix(etag, `"`) + `-gzip"`
}

// shouldCompress reports whether the file response should be compressed. Block requests
// have a Range and exact byte offsets; a conditional request names the plain file by its
// tag. Neither may be answered with a compressed body.
func shouldCompress(r *http.Request, contentType string, size int64) bool {
	if size < minCompressSize || !compressible(contentType) || r.Header.Get("Range") != "" {
		return false
	}
	for _, name := range validatorHeaders {
		if r.Header.Get(name) != "" {
			return false
		}
	}
	return acceptsGzip(r.Header)
}

// multipleRanges reports whether a Range header asks for more than one range.
func multipleRanges(header string) bool {
	specs, ok := strings.CutPrefix(header, "bytes=")
	if !ok {
		return false
	}
	count := 0
	for spec := range strings.SplitSeq(specs, ",") {
		if strings.TrimSpace(spec) != "" {
			count++
		}
	}
	return count > 1
}
