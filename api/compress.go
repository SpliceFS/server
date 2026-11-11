package api

import (
	"compress/gzip"
	"errors"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// minCompressSize is the smallest body that is worth compressing.
const minCompressSize = 1024

// compressibleTypes are the media types, besides text/* and the +json and +xml
// suffixes, whose content is compressed.
var compressibleTypes = []string{
	"application/json",
	"application/xml",
	"application/javascript",
	"image/svg+xml",
}

// compressible reports whether a Content-Type value names a type that compresses well.
func compressible(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return strings.HasPrefix(mediaType, "text/") ||
		slices.Contains(compressibleTypes, mediaType) ||
		strings.HasSuffix(mediaType, "+json") ||
		strings.HasSuffix(mediaType, "+xml")
}

// acceptsGzip reports whether the Accept-Encoding headers allow gzip: an entry for gzip
// with a quality above zero, or, when there is no entry for gzip, a wildcard entry with a
// quality above zero.
func acceptsGzip(h http.Header) bool {
	gzipQuality, wildcardQuality := -1.0, -1.0
	for _, line := range h.Values("Accept-Encoding") {
		for item := range strings.SplitSeq(line, ",") {
			coding, quality := parseCoding(item)
			switch coding {
			case "gzip":
				gzipQuality = quality
			case "*":
				wildcardQuality = quality
			}
		}
	}
	if gzipQuality >= 0 {
		return gzipQuality > 0
	}
	return wildcardQuality > 0
}

// parseCoding splits one Accept-Encoding item into its lower case coding and its quality.
// The quality is 1 when absent and 0 when it cannot be read, so that a garbled item
// never grants compression.
func parseCoding(item string) (coding string, quality float64) {
	name, params, _ := strings.Cut(item, ";")
	quality = 1
	for param := range strings.SplitSeq(params, ";") {
		key, value, ok := strings.Cut(param, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "q") {
			continue
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			parsed = 0
		}
		quality = parsed
	}
	return strings.ToLower(strings.TrimSpace(name)), quality
}

// writeGzip sends a 200 response with the gzip compressed content of src. A compressed
// body has no known length, so none is announced.
func writeGzip(w http.ResponseWriter, src io.Reader) error {
	w.Header().Set("Content-Encoding", "gzip")
	w.WriteHeader(http.StatusOK)
	zw := gzip.NewWriter(w)
	_, err := io.Copy(zw, src)
	return errors.Join(err, zw.Close())
}
