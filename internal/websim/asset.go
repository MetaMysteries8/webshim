package websim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
)

// MaxAssetBytes is the local upload ceiling, 500 MiB. The playbook is explicit
// that this is a local safety policy, not a documented WebSim platform limit.
const MaxAssetBytes = 524_288_000

// ListAssets returns the assets of a revision.
func (c *Client) ListAssets(ctx context.Context, projectID string, version int) ([]Asset, error) {
	body, err := c.getJSON(ctx, "list assets",
		"/projects/"+projectID+"/revisions/"+strconv.Itoa(version)+"/assets")
	if err != nil {
		return nil, err
	}
	return normAssets(body)
}

// assetMetadata is one entry of the multipart "contents" field.
type assetMetadata struct {
	Size            int64  `json:"size"`
	ExistingAssetID string `json:"existingAssetId,omitempty"`
}

// WriteAsset creates or replaces a non-index asset in a draft revision
// (Flow D).
//
// It resolves an existing asset ID by exact path before uploading, so a write
// to an existing path replaces rather than duplicates. Playbook rule 6: never
// overwrite an asset without first resolving its existing asset ID.
//
// index.html is rejected: the homepage is written through POST /sites.
func (c *Client) WriteAsset(ctx context.Context, projectID string, version int, assetPath string, content []byte) (*Asset, error) {
	cleanPath, err := ValidatePath(assetPath)
	if err != nil {
		return nil, err
	}
	if IsIndexPath(cleanPath) {
		return nil, fmt.Errorf("refusing to upload %s as an asset: the homepage is written through POST /sites", IndexPath)
	}
	if int64(len(content)) > MaxAssetBytes {
		return nil, fmt.Errorf("asset %s is %d bytes, over the %d byte local limit",
			cleanPath, len(content), MaxAssetBytes)
	}

	existing, err := c.ListAssets(ctx, projectID, version)
	if err != nil {
		return nil, fmt.Errorf("resolving existing asset id: %w", err)
	}
	meta := assetMetadata{Size: int64(len(content))}
	if prior := findAssetByPath(existing, cleanPath); prior != nil {
		meta.ExistingAssetID = prior.ID
	}

	body, contentType, err := buildAssetForm(meta, cleanPath, content)
	if err != nil {
		return nil, err
	}

	if _, err := c.do(ctx, request{
		op:          "upload asset",
		method:      http.MethodPost,
		url:         c.apiURL("/projects/" + projectID + "/revisions/" + strconv.Itoa(version) + "/assets"),
		contentType: contentType,
		newBody:     func() (io.Reader, error) { return bytes.NewReader(body), nil },
	}); err != nil {
		return nil, err
	}

	// Verify by exact path (Flow D step 4).
	after, err := c.ListAssets(ctx, projectID, version)
	if err != nil {
		return nil, fmt.Errorf("verifying asset upload: %w", err)
	}
	written := findAssetByPath(after, cleanPath)
	if written == nil {
		return nil, fmt.Errorf("%w: asset %q is absent after a successful upload", ErrUnexpectedShape, cleanPath)
	}
	if written.Size != 0 && written.Size != int64(len(content)) {
		return nil, fmt.Errorf("%w: asset %q reports size %d but %d bytes were uploaded",
			ErrUnexpectedShape, cleanPath, written.Size, len(content))
	}
	return written, nil
}

// buildAssetForm assembles the multipart body.
//
// The field names are fixed by the API: "contents" carries a JSON array of
// metadata, and "0" carries the first file. The returned content type comes
// from the writer that produced the body, so the boundary always matches.
func buildAssetForm(meta assetMetadata, assetPath string, content []byte) (body []byte, contentType string, err error) {
	metaJSON, err := json.Marshal([]assetMetadata{meta})
	if err != nil {
		return nil, "", fmt.Errorf("encoding asset metadata: %w", err)
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	if err := w.WriteField("contents", string(metaJSON)); err != nil {
		return nil, "", fmt.Errorf("writing contents field: %w", err)
	}

	// CreatePart rather than CreateFormFile: the latter hardcodes
	// application/octet-stream, and the API is given the resolved MIME type.
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="0"; filename="%s"`, escapeQuotes(assetPath)))
	h.Set("Content-Type", DetectMIME(assetPath, content))

	part, err := w.CreatePart(h)
	if err != nil {
		return nil, "", fmt.Errorf("creating file part: %w", err)
	}
	if _, err := part.Write(content); err != nil {
		return nil, "", fmt.Errorf("writing file part: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("closing multipart writer: %w", err)
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// escapeQuotes makes a filename safe for a Content-Disposition header. This
// mirrors what mime/multipart does internally for form fields.
var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"")

func escapeQuotes(s string) string { return quoteEscaper.Replace(s) }

// DeleteAsset removes one asset from a revision by exact path (Flow E).
//
// The path must exist and must match exactly. The playbook forbids broadening a
// deletion by prefix, wildcard, or fuzzy matching, and index.html is never
// deleted as a normal asset operation.
func (c *Client) DeleteAsset(ctx context.Context, projectID string, version int, assetPath string) error {
	cleanPath, err := ValidatePath(assetPath)
	if err != nil {
		return err
	}
	if IsIndexPath(cleanPath) {
		return fmt.Errorf("refusing to delete %s: the homepage is not a normal asset", IndexPath)
	}

	before, err := c.ListAssets(ctx, projectID, version)
	if err != nil {
		return err
	}
	if findAssetByPath(before, cleanPath) == nil {
		return fmt.Errorf("%w: no asset at exact path %q in revision %d", ErrNotFound, cleanPath, version)
	}

	if _, err := c.sendJSON(ctx, "delete asset", http.MethodPost,
		"/projects/"+projectID+"/revisions/"+strconv.Itoa(version)+"/edit-assets",
		map[string]any{
			"operation": map[string]any{
				"type": "delete",
				"path": cleanPath,
			},
		}); err != nil {
		return err
	}

	after, err := c.ListAssets(ctx, projectID, version)
	if err != nil {
		return fmt.Errorf("verifying asset deletion: %w", err)
	}
	if findAssetByPath(after, cleanPath) != nil {
		return fmt.Errorf("%w: asset %q is still present after deletion", ErrUnexpectedShape, cleanPath)
	}
	return nil
}
