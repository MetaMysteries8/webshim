package websim

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// IndexPath is the project homepage. It is written through POST /sites, never
// through the asset endpoint.
const IndexPath = "index.html"

// duplicateIndexRe matches the forbidden "index (1).html" family of duplicate
// homepages, case-insensitively. Playbook rule 9.
var duplicateIndexRe = regexp.MustCompile(`(?i)(^|/)index \(\d+\)\.html$`)

// ValidatePath enforces the playbook's path rules. It rejects paths that are
// absolute, contain a component exactly equal to "..", or name a duplicate
// homepage.
//
// It returns the cleaned path on success.
func ValidatePath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("%w: path is empty", ErrUnsafePath)
	}

	// Normalize separators so a Windows-style path cannot smuggle a ".."
	// component past the component check below.
	unified := strings.ReplaceAll(p, `\`, "/")

	if strings.HasPrefix(unified, "/") {
		return "", fmt.Errorf("%w: %q is absolute", ErrUnsafePath, p)
	}
	// A Windows drive letter (C:/...) is absolute too.
	if len(unified) >= 2 && unified[1] == ':' {
		return "", fmt.Errorf("%w: %q is absolute", ErrUnsafePath, p)
	}

	for _, seg := range strings.Split(unified, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: %q contains a %q component", ErrUnsafePath, p, "..")
		}
	}

	if duplicateIndexRe.MatchString(unified) {
		return "", fmt.Errorf("%w: %q is a duplicate homepage; the homepage is %s and is written through POST /sites",
			ErrUnsafePath, p, IndexPath)
	}

	cleaned := path.Clean(unified)
	if cleaned == "." || cleaned == "/" {
		return "", fmt.Errorf("%w: %q does not name a file", ErrUnsafePath, p)
	}
	// path.Clean can still surface a leading ".." when the input escaped
	// upward via a leading "./..".
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: %q escapes the project root", ErrUnsafePath, p)
	}
	return cleaned, nil
}

// IsIndexPath reports whether p names the project homepage.
func IsIndexPath(p string) bool {
	return strings.EqualFold(strings.TrimPrefix(p, "./"), IndexPath)
}

// encodeContentPath URL-encodes each path segment independently, preserving the
// "/" separators. Playbook: "Every path segment should be URL-encoded
// independently. Do not encode `/` as part of a single whole-path operation."
func encodeContentPath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// mimeByExtension is the MIME table used by the local agent, transcribed from
// the playbook's Flow D.
var mimeByExtension = map[string]string{
	".html": "text/html; charset=utf-8",
	".css":  "text/css",
	".js":   "text/javascript",
	".mjs":  "text/javascript",
	".json": "application/json",
	".md":   "text/markdown",
	".txt":  "text/plain",
	".svg":  "image/svg+xml",
	".xml":  "application/xml",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
	".avif": "image/avif",
	".mp4":  "video/mp4",
	".webm": "video/webm",
	".mov":  "video/quicktime",
	".m4v":  "video/mp4",
}

// DetectMIME resolves a content type for an asset.
//
// Known extensions use the playbook's table. For unknown extensions the
// playbook notes the local agent defaults to text/plain but that "an agent
// should determine the real MIME type instead of assuming text when possible",
// so this sniffs the bytes rather than mislabelling binary data as text.
func DetectMIME(assetPath string, content []byte) string {
	ext := strings.ToLower(path.Ext(assetPath))
	if mt, ok := mimeByExtension[ext]; ok {
		return mt
	}
	if len(content) > 0 {
		if sniffed := http.DetectContentType(content); sniffed != "" {
			return sniffed
		}
	}
	return "text/plain"
}
