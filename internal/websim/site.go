package websim

import (
	"context"
	"fmt"
	"net/http"
)

// WriteIndex writes or replaces the project homepage for a draft revision
// (Flow C).
//
// index.html is not uploaded through the asset endpoint; POST /sites is the
// documented write path, which is also why this implementation never produces
// an "index (1).html" duplicate.
//
// description becomes prompt_data_override.text and is visible in the project's
// history. It must not contain private prompts, tokens, hidden administrator
// rules, or personal data.
func (c *Client) WriteIndex(ctx context.Context, projectID string, revision *Revision, html, description string) (siteID string, err error) {
	revisionID, version, err := revision.RequireIdentity()
	if err != nil {
		return "", err
	}
	if description == "" {
		description = "automated homepage edit"
	}

	body, err := c.sendJSON(ctx, "write index.html", http.MethodPost, "/sites",
		map[string]any{
			"project_id":          projectID,
			"project_version":     version,
			"project_revision_id": revisionID,
			"content":             html,
			"prompt_data_override": map[string]any{
				"type": "plaintext",
				"text": c.san.clean(description),
				"data": nil,
			},
		})
	if err != nil {
		return "", err
	}

	siteID, err = normSiteID(body)
	if err != nil {
		return "", fmt.Errorf("writing %s: %w", IndexPath, err)
	}
	return siteID, nil
}
