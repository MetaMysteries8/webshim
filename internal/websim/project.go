package websim

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// GetProject reads project metadata. It tolerates both the {"project": {...}}
// envelope and a bare project object.
func (c *Client) GetProject(ctx context.Context, projectID string) (*Project, error) {
	body, err := c.getJSON(ctx, "get project", "/projects/"+projectID)
	if err != nil {
		return nil, err
	}
	return normProject(body)
}

// createBlankProject creates a project together with its initial draft revision
// (Flow G step 1). It returns the project and that first revision.
//
// It is unexported because a project with no homepage is not a useful artifact;
// the full lifecycle lives in Client.CreateProject.
func (c *Client) createBlankProject(ctx context.Context, visibility string) (*Project, *Revision, error) {
	if visibility == "" {
		visibility = "public"
	}
	body, err := c.sendJSON(ctx, "create project", http.MethodPost,
		"/projects?include[]=permissions",
		map[string]any{"visibility": visibility})
	if err != nil {
		return nil, nil, err
	}

	project, err := normProject(body)
	if err != nil {
		return nil, nil, err
	}
	revision, err := normSingleRevision(body)
	if err != nil {
		return nil, nil, err
	}
	if _, _, err := revision.RequireIdentity(); err != nil {
		return nil, nil, err
	}
	return project, revision, nil
}

// ProjectMetadata holds the project fields the playbook documents as writable.
// Nil fields are omitted from the PATCH body, so a caller can update one field
// without disturbing the others.
type ProjectMetadata struct {
	Title        *string
	Description  *string
	Slug         *string
	Visibility   *string
	CommentsMode *string
	EnableChat   *bool
	Posted       *bool
}

// slugUnsafeRe matches everything a slug may not contain.
var slugUnsafeRe = regexp.MustCompile(`[^a-z0-9-]+`)

// SanitizeSlug lowercases a string and reduces it to letters, numbers, and
// single hyphens.
//
// Passing sanitization does not mean the slug is available: the playbook warns
// that a PATCH being accepted is not proof the slug was granted, so callers
// must inspect the returned project.
func SanitizeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugUnsafeRe.ReplaceAllString(s, "-")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return strings.Trim(s, "-")
}

// UpdateProjectMetadata patches project metadata and returns the server's view
// of the project afterwards.
//
// Callers that set Slug must compare the returned project's Slug against what
// they asked for; the request being accepted does not prove the slug was
// granted.
func (c *Client) UpdateProjectMetadata(ctx context.Context, projectID string, meta ProjectMetadata) (*Project, error) {
	payload := map[string]any{}
	if meta.Title != nil {
		payload["title"] = *meta.Title
	}
	if meta.Description != nil {
		payload["description"] = *meta.Description
	}
	if meta.Slug != nil {
		clean := SanitizeSlug(*meta.Slug)
		if clean == "" {
			return nil, fmt.Errorf("slug %q reduces to empty after sanitization", *meta.Slug)
		}
		payload["slug"] = clean
	}
	if meta.Visibility != nil {
		payload["visibility"] = *meta.Visibility
	}
	if meta.CommentsMode != nil {
		payload["comments_mode"] = *meta.CommentsMode
	}
	if meta.EnableChat != nil {
		payload["enable_chat"] = *meta.EnableChat
	}
	if meta.Posted != nil {
		payload["posted"] = *meta.Posted
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("update project metadata: no fields to update")
	}

	body, err := c.sendJSON(ctx, "update project metadata", http.MethodPatch,
		"/projects/"+projectID+"?include[]=permissions", payload)
	if err != nil {
		return nil, err
	}
	return normProject(body)
}

// setCurrentVersion points the project at a published revision.
//
// This is the only call that changes what visitors see. It is unexported: every
// path to it (Publish, Rollback) must first prove the target revision is
// finalized, and must verify afterwards.
func (c *Client) setCurrentVersion(ctx context.Context, projectID string, version int) error {
	_, err := c.sendJSON(ctx, "set current version", http.MethodPatch,
		"/projects/"+projectID,
		map[string]any{
			"current_version":  version,
			"auto_set_current": false,
		})
	return err
}

// verifyCurrentVersion re-reads the project and requires current_version to
// equal want. Playbook: "Never declare success until GET /projects/{id}
// confirms current_version."
func (c *Client) verifyCurrentVersion(ctx context.Context, projectID string, want int) error {
	project, err := c.GetProject(ctx, projectID)
	if err != nil {
		return fmt.Errorf("verifying current version: %w", err)
	}
	got, err := project.RequireCurrentVersion()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("promotion verification failed: expected current_version %d, got %d", want, got)
	}
	return nil
}
