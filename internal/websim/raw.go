package websim

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
)

// ContentURL builds the raw content-host URL for a file in a revision.
//
// Each path segment is URL-encoded independently so that "/" separators
// survive, per the playbook.
func (c *Client) ContentURL(projectID, filePath string, version int) string {
	return c.contentHostFn(projectID) + "/" + encodeContentPath(filePath) +
		"?v=" + strconv.Itoa(version) + "&raw="
}

// contentReferer is the Referer the content host expects.
func contentReferer(projectID string, version int) string {
	return origin + "/p/" + projectID + "/" + strconv.Itoa(version)
}

// ReadFile downloads a file from the raw content host.
//
// The content host is not authenticated in this implementation, so no bearer
// token is sent to it. A HEAD request runs first to enforce the size limit
// before any bytes are transferred, and the limit is enforced again on the
// actual body -- a Content-Length can lie.
func (c *Client) ReadFile(ctx context.Context, projectID, filePath string, version int) ([]byte, error) {
	cleanPath, err := ValidatePath(filePath)
	if err != nil {
		return nil, err
	}

	rawURL := c.ContentURL(projectID, cleanPath, version)
	ref := contentReferer(projectID, version)

	// HEAD first so an oversized file is refused before it is downloaded.
	// A missing or unparseable Content-Length is not fatal here: the body
	// read below is capped regardless.
	if size, ok, err := c.headContentLength(ctx, rawURL, ref); err != nil {
		return nil, err
	} else if ok && size > MaxAssetBytes {
		return nil, fmt.Errorf("%s reports %d bytes, over the %d byte local limit",
			cleanPath, size, MaxAssetBytes)
	}

	body, err := c.do(ctx, request{
		op:      "read raw file",
		method:  http.MethodGet,
		url:     rawURL,
		referer: ref,
		noAuth:  true,
	})
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > MaxAssetBytes {
		return nil, fmt.Errorf("%s returned %d bytes, over the %d byte local limit",
			cleanPath, len(body), MaxAssetBytes)
	}
	return body, nil
}

// headContentLength issues a HEAD and reports the declared size. ok is false
// when the server did not provide a usable Content-Length.
func (c *Client) headContentLength(ctx context.Context, rawURL, referer string) (size int64, ok bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return 0, false, fmt.Errorf("building HEAD request: %w", err)
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Referer", referer)

	resp, err := c.http.Do(req)
	if err != nil {
		// A failed HEAD is not fatal; the capped GET below is the real
		// guard. Report it and continue.
		c.log.Debug("HEAD on content host failed; continuing to GET",
			"url", c.san.clean(rawURL), "error", c.san.clean(err.Error()))
		return 0, false, nil
	}
	defer resp.Body.Close() //nolint:errcheck // no body expected

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, false, &APIError{
			Op:         "head raw file",
			Method:     http.MethodHead,
			URL:        c.san.clean(rawURL),
			StatusCode: resp.StatusCode,
		}
	}
	if resp.ContentLength < 0 {
		return 0, false, nil
	}
	return resp.ContentLength, true, nil
}
