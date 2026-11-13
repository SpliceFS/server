package api

import (
	"compress/gzip"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// minCompressSize is the smallest body that is worth compressing.
const minCompressSize = 1024

// compressible reports whether a Content-Type value names a type that compresses well.
func compressible(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return strings.HasPrefix(mediaType, "text/") ||
		compressibleType(mediaType) ||
		strings.HasSuffix(mediaType, "+json") ||
		strings.HasSuffix(mediaType, "+xml")
}

// compressibleType reports whether mediaType is one of the types, besides text/* and the
// +json and +xml suffixes, whose content is compressed.
func compressibleType(mediaType string) bool {
	switch mediaType {
	case "application/json", "application/xml", "application/javascript", "image/svg+xml":
		return true
	default:
		return false
	}
}

// acceptsGzip reports whether the Accept-Encoding headers allow gzip: an entry for gzip
// with a quality above zero, or, when there is no entry for gzip, a wildcard entry with a
// quality above zero.
func acceptsGzip(header http.Header) bool {
	gzipQuality, wildcardQuality := -1.0, -1.0
	for _, line := range header.Values("Accept-Encoding") {
		for item := range strings.SplitSeq(line, ",") {
			entry := parseCoding(item)
			switch entry.coding {
			case "gzip":
				gzipQuality = entry.quality
			case "*":
				wildcardQuality = entry.quality
			default:
				// Other codings do not decide whether gzip is allowed.
			}
		}
	}
	if gzipQuality >= 0 {
		return gzipQuality > 0
	}
	return wildcardQuality > 0
}

// weightedCoding is one item of an Accept-Encoding header.
type weightedCoding struct {
	// coding is the lower case name of the content coding.
	coding string
	// quality is 1 when the item has no q parameter and 0 when the parameter cannot be
	// read, so that a garbled item never grants compression.
	quality float64
}

// parseCoding splits one Accept-Encoding item into its coding and its quality.
func parseCoding(item string) weightedCoding {
	name, params, _ := strings.Cut(item, ";")
	quality := 1.0
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
	return weightedCoding{coding: strings.ToLower(strings.TrimSpace(name)), quality: quality}
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
