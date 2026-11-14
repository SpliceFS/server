package api

import (
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/SpliceFS/common/wire"
	"github.com/SpliceFS/server/share"
)

// defaultContentType is used for a file whose extension is not known.
const defaultContentType = "application/octet-stream"

// file answers GET and HEAD /v1/file/{share}/{name...}.
func (s *server) file(w http.ResponseWriter, r *http.Request, shared Share) {
	name := r.PathValue("name")
	if r.Method == http.MethodHead {
		s.head(w, r, shared, name)
		return
	}
	file, err := shared.Open(name)
	if err != nil {
		s.failLookup(w, r, err, share.ErrNotFound, share.ErrIsDir)
		return
	}
	//nolint:errcheck // The file was only read, so a failed close loses nothing and the response is already sent.
	defer file.Close()
	s.get(w, r, file)
}

// head answers HEAD with the headers a GET would carry, plus the head and tail digests.
func (s *server) head(w http.ResponseWriter, r *http.Request, shared Share, name string) {
	digests, info, err := shared.Digests(name)
	if err != nil {
		s.failLookup(w, r, err, share.ErrNotFound, share.ErrIsDir)
		return
	}
	header := w.Header()
	setFileHeaders(header, info)
	header.Set("Content-Length", strconv.FormatInt(info.Size, 10))
	header.Set("Accept-Ranges", "bytes")
	header.Set(wire.HeaderHeadDigest, digests.Head)
	header.Set(wire.HeaderTailDigest, digests.Tail)
	w.WriteHeader(http.StatusOK)
}

// get sends file, which is the file that the ETag and the Last-Modified time
// describe, so that the preconditions of the request are checked against it.
func (s *server) get(w http.ResponseWriter, r *http.Request, file File) {
	info := file.Info()
	contentType := setFileHeaders(w.Header(), info)
	switch {
	case multipleRanges(r.Header.Get("Range")):
		// A multipart answer costs the server little and the client a lot to ask for
		// many tiny ranges; the API serves one range per request.
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(info.Size, 10))
		writeError(w, http.StatusRequestedRangeNotSatisfiable, errorMessage(http.StatusRequestedRangeNotSatisfiable))
	case shouldCompress(r, contentType, info.Size):
		s.sendCompressed(w, r, file, info)
	default:
		// The Content-Type is already set, so ServeContent does not sniff the content.
		http.ServeContent(&errorBodyWriter{ResponseWriter: w}, r, "", info.MTime, file)
	}
}

// sendCompressed sends the whole file with gzip. It is a different representation of
// the file, so it gets a different tag: a block request that holds the tag of the plain
// file must never be satisfied by it.
func (s *server) sendCompressed(w http.ResponseWriter, r *http.Request, file File, info share.Info) {
	w.Header().Set("ETag", compressedETag(info.ETag))
	s.sendGzip(w, r, file)
}

// setFileHeaders sets the headers that describe a file and do not depend on the request,
// and returns the content type it chose.
func setFileHeaders(header http.Header, info share.Info) string {
	contentType := contentTypeOf(info.Name)
	header.Set("Content-Type", contentType)
	header.Set("ETag", info.ETag)
	header.Set("Last-Modified", info.MTime.UTC().Format(http.TimeFormat))
	if compressible(contentType) {
		header.Add("Vary", "Accept-Encoding")
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
	return !hasValidator(r.Header) && acceptsGzip(r.Header)
}

// hasValidator reports whether the request has a header that makes the answer depend on
// the exact representation of the file.
func hasValidator(header http.Header) bool {
	for _, name := range [...]string{
		"If-Match", "If-None-Match", "If-Range", "If-Modified-Since", "If-Unmodified-Since",
	} {
		if header.Get(name) != "" {
			return true
		}
	}
	return false
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
