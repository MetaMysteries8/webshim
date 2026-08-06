package websim

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
)

// ListRevisions returns every revision of a project, normalized from
// revisions.data[].project_revision.
func (c *Client) ListRevisions(ctx context.Context, projectID string) ([]Revision, error) {
	body, err := c.getJSON(ctx, "list revisions", "/projects/"+projectID+"/revisions")
	if err != nil {
		return nil, err
	}
	return normRevisions(body)
}

// GetRevision returns a single revision by version number.
func (c *Client) GetRevision(ctx context.Context, projectID string, version int) (*Revision, error) {
	revs, err := c.ListRevisions(ctx, projectID)
	if err != nil {
		return nil, err
	}
	rev := findRevision(revs, version)
	if rev == nil {
		return nil, fmt.Errorf("%w: revision %d of project %s", ErrNotFound, version, projectID)
	}
	return rev, nil
}

// requirePublishedParent proves a version exists and is finalized, so it is
// legal to branch from. Playbook rule 3: never edit a finalized revision;
// branch a new draft from a published parent.
func (c *Client) requirePublishedParent(ctx context.Context, projectID string, version int) error {
	rev, err := c.GetRevision(ctx, projectID, version)
	if err != nil {
		return fmt.Errorf("locating parent revision: %w", err)
	}
	if rev.Draft {
		return fmt.Errorf("parent revision %d of project %s is still a draft; refusing to branch from it",
			version, projectID)
	}
	return nil
}

// CreateDraft branches a new draft revision from a published parent.
func (c *Client) CreateDraft(ctx context.Context, projectID string, parentVersion int) (*Revision, error) {
	body, err := c.sendJSON(ctx, "create draft revision", http.MethodPost,
		"/projects/"+projectID+"/revisions",
		map[string]any{"parent_version": parentVersion})
	if err != nil {
		return nil, err
	}

	rev, err := normSingleRevision(body)
	if err != nil {
		return nil, err
	}
	if _, _, err := rev.RequireIdentity(); err != nil {
		return nil, err
	}
	if !rev.Draft {
		return nil, fmt.Errorf("%w: newly created revision %d is not marked draft",
			ErrUnexpectedShape, *rev.Version)
	}
	return rev, nil
}

// FinalizeRevision sets draft=false, then re-lists revisions to prove it took.
//
// Playbook step 7: if the revision remains a draft, do not promote. Returning
// an error here is what keeps a draft from ever becoming live (rule 4).
func (c *Client) FinalizeRevision(ctx context.Context, projectID string, version int) error {
	_, err := c.sendJSON(ctx, "finalize revision", http.MethodPatch,
		"/projects/"+projectID+"/revisions/"+strconv.Itoa(version),
		map[string]any{"draft": false})
	if err != nil {
		return err
	}

	rev, err := c.GetRevision(ctx, projectID, version)
	if err != nil {
		return fmt.Errorf("verifying finalization: %w", err)
	}
	if rev.Draft {
		return fmt.Errorf("revision %d of project %s is still a draft after finalization; refusing to promote",
			version, projectID)
	}
	return nil
}

// ListEditHistory returns the edit history of a revision.
func (c *Client) ListEditHistory(ctx context.Context, projectID string, version int) ([]Edit, error) {
	body, err := c.getJSON(ctx, "get edit history",
		"/projects/"+projectID+"/revisions/"+strconv.Itoa(version)+"/edit-history")
	if err != nil {
		return nil, err
	}
	return normEdits(body)
}

// RollbackResult records a completed rollback.
type RollbackResult struct {
	OK                 bool   `json:"ok"`
	Action             string `json:"action"`
	ProjectID          string `json:"project_id"`
	PreRollbackVersion int    `json:"pre_rollback_version"`
	CurrentVersion     int    `json:"current_version"`
	Verified           bool   `json:"verified"`
}

// Rollback makes an existing published revision current (Flow F).
//
// Rollback selects a revision. It never deletes newer revisions -- the playbook
// is explicit that deletion is not an inferred recovery action.
func (c *Client) Rollback(ctx context.Context, projectID string, targetVersion int) (*RollbackResult, error) {
	var result *RollbackResult

	err := c.WithProjectLock(ctx, projectID, func(ctx context.Context) error {
		project, err := c.GetProject(ctx, projectID)
		if err != nil {
			return err
		}
		preRollback, err := project.RequireCurrentVersion()
		if err != nil {
			return err
		}

		target, err := c.GetRevision(ctx, projectID, targetVersion)
		if err != nil {
			return err
		}
		if target.Draft {
			return fmt.Errorf("revision %d is a draft; only published revisions may be made current",
				targetVersion)
		}

		if err := c.setCurrentVersion(ctx, projectID, targetVersion); err != nil {
			return err
		}
		if err := c.verifyCurrentVersion(ctx, projectID, targetVersion); err != nil {
			return err
		}

		result = &RollbackResult{
			OK:                 true,
			Action:             "rollback",
			ProjectID:          projectID,
			PreRollbackVersion: preRollback,
			CurrentVersion:     targetVersion,
			Verified:           true,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
