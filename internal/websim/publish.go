package websim

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// Change is a single file write in a publish.
type Change struct {
	// Path is project-relative. The value IndexPath routes to POST /sites;
	// everything else is uploaded as an asset.
	Path string

	// Content is the exact bytes to write.
	Content []byte
}

// PublishRequest describes one atomic edit-and-publish.
type PublishRequest struct {
	ProjectID string

	// Writes are applied before Deletes.
	Writes []Change

	// Deletes are project-relative paths to remove from the new revision.
	// index.html cannot be deleted.
	Deletes []string

	// Description becomes prompt_data_override.text when the homepage is
	// written. It is public: no secrets, no private prompts.
	Description string
}

func (r PublishRequest) validate() error {
	if r.ProjectID == "" {
		return errors.New("publish: project id is required")
	}
	if len(r.Writes) == 0 && len(r.Deletes) == 0 {
		return errors.New("publish: nothing to write or delete")
	}
	seen := make(map[string]bool, len(r.Writes))
	for _, w := range r.Writes {
		clean, err := ValidatePath(w.Path)
		if err != nil {
			return err
		}
		if seen[clean] {
			return fmt.Errorf("publish: path %q appears more than once", clean)
		}
		seen[clean] = true
	}
	for _, d := range r.Deletes {
		clean, err := ValidatePath(d)
		if err != nil {
			return err
		}
		if IsIndexPath(clean) {
			return fmt.Errorf("publish: refusing to delete %s", IndexPath)
		}
		if seen[clean] {
			return fmt.Errorf("publish: path %q is both written and deleted", clean)
		}
	}
	return nil
}

// changedPaths returns every path this request touches, sorted, for the result
// record.
func (r PublishRequest) changedPaths() []string {
	out := make([]string, 0, len(r.Writes)+len(r.Deletes))
	for _, w := range r.Writes {
		out = append(out, w.Path)
	}
	out = append(out, r.Deletes...)
	sort.Strings(out)
	return out
}

// Publish runs the full edit lifecycle (Flow B) as one transaction.
//
//	read current version -> branch a draft -> write -> finalize -> verify
//	-> re-check for concurrent changes -> promote -> verify
//
// The per-project lock is held across the whole sequence, not just the upload,
// because a lock around only the write step cannot prevent two flows from
// branching off the same parent and clobbering each other's intent.
//
// Every failure before promotion leaves the previous live version untouched and
// wraps ErrNotPromoted. The draft is left in place: the playbook forbids
// deleting a failed draft as automatic cleanup, since it is useful for
// diagnosis.
func (c *Client) Publish(ctx context.Context, req PublishRequest) (*PublishResult, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}

	var result *PublishResult
	err := c.WithProjectLock(ctx, req.ProjectID, func(ctx context.Context) error {
		// Step 1: read the current live version.
		project, err := c.GetProject(ctx, req.ProjectID)
		if err != nil {
			return notPromoted(err)
		}
		previousVersion, err := project.RequireCurrentVersion()
		if err != nil {
			return notPromoted(err)
		}

		// Step 2: the parent must be published before we branch from it.
		if err := c.requirePublishedParent(ctx, req.ProjectID, previousVersion); err != nil {
			return notPromoted(err)
		}

		// Step 3: branch a draft.
		draft, err := c.CreateDraft(ctx, req.ProjectID, previousVersion)
		if err != nil {
			return notPromoted(err)
		}
		_, newVersion, err := draft.RequireIdentity()
		if err != nil {
			return notPromoted(err)
		}

		c.log.Info("draft created",
			"project", req.ProjectID, "parent_version", previousVersion, "draft_version", newVersion)

		// Step 4: apply edits to the draft.
		if err := c.applyChanges(ctx, req, draft); err != nil {
			return notPromoted(err)
		}

		// Steps 6 and 7: finalize, and prove it took.
		if err := c.FinalizeRevision(ctx, req.ProjectID, newVersion); err != nil {
			return notPromoted(err)
		}

		// Rebase guard. Between step 1 and here another actor may have moved
		// current_version. Promoting now would silently discard their work,
		// so stop and let the caller rebase onto the new parent.
		if err := c.requireUnchangedSince(ctx, req.ProjectID, previousVersion); err != nil {
			return notPromoted(err)
		}

		// Steps 8 and 9: promote and verify.
		if err := c.promoteAndVerify(ctx, req.ProjectID, previousVersion, newVersion); err != nil {
			return err // already carries the right promotion semantics
		}

		result = &PublishResult{
			OK:              true,
			Action:          "publish_edit",
			ProjectID:       req.ProjectID,
			PreviousVersion: previousVersion,
			CurrentVersion:  newVersion,
			ChangedPaths:    req.changedPaths(),
			Verified:        true,
		}
		c.log.Info("published",
			"project", req.ProjectID,
			"previous_version", previousVersion,
			"current_version", newVersion,
			"changed_paths", result.ChangedPaths)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// applyChanges writes every file and applies every deletion to a draft.
func (c *Client) applyChanges(ctx context.Context, req PublishRequest, draft *Revision) error {
	_, version, err := draft.RequireIdentity()
	if err != nil {
		return err
	}

	for _, w := range req.Writes {
		clean, err := ValidatePath(w.Path)
		if err != nil {
			return err
		}
		if IsIndexPath(clean) {
			if _, err := c.WriteIndex(ctx, req.ProjectID, draft, string(w.Content), req.Description); err != nil {
				return fmt.Errorf("writing %s: %w", IndexPath, err)
			}
			continue
		}
		if _, err := c.WriteAsset(ctx, req.ProjectID, version, clean, w.Content); err != nil {
			return fmt.Errorf("writing %s: %w", clean, err)
		}
	}

	for _, d := range req.Deletes {
		clean, err := ValidatePath(d)
		if err != nil {
			return err
		}
		if err := c.DeleteAsset(ctx, req.ProjectID, version, clean); err != nil {
			return fmt.Errorf("deleting %s: %w", clean, err)
		}
	}
	return nil
}

// requireUnchangedSince fails when current_version has moved away from want.
func (c *Client) requireUnchangedSince(ctx context.Context, projectID string, want int) error {
	project, err := c.GetProject(ctx, projectID)
	if err != nil {
		return fmt.Errorf("re-checking for concurrent changes: %w", err)
	}
	got, err := project.RequireCurrentVersion()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%w: current_version moved from %d to %d while this edit was in flight; "+
			"rebase by branching a new draft from %d",
			ErrConcurrentModification, want, got, got)
	}
	return nil
}

// promoteAndVerify points the project at newVersion and proves it took.
//
// A failed PATCH is ambiguous: the change may have been committed even though
// the response was lost. This re-reads state to find out, rather than retrying
// blind.
func (c *Client) promoteAndVerify(ctx context.Context, projectID string, previousVersion, newVersion int) error {
	promoteErr := c.setCurrentVersion(ctx, projectID, newVersion)
	if promoteErr == nil {
		if err := c.verifyCurrentVersion(ctx, projectID, newVersion); err != nil {
			return err
		}
		return nil
	}

	// Find out what actually happened before deciding anything.
	project, err := c.GetProject(ctx, projectID)
	if err != nil {
		return fmt.Errorf("promotion failed and project state could not be read: %w (original error: %v)",
			err, promoteErr)
	}
	current, err := project.RequireCurrentVersion()
	if err != nil {
		return err
	}

	switch current {
	case newVersion:
		// The write landed; only the response was lost.
		c.log.Warn("promotion response was lost but the change was applied",
			"project", projectID, "current_version", newVersion)
		return nil
	case previousVersion:
		return fmt.Errorf("%w: promotion did not take; live version is still %d: %v",
			ErrNotPromoted, previousVersion, promoteErr)
	default:
		return fmt.Errorf("%w: promotion failed and current_version is %d, which is neither %d nor %d; "+
			"another actor changed this project. Not retrying. (original error: %v)",
			ErrConcurrentModification, current, previousVersion, newVersion, promoteErr)
	}
}

// notPromoted marks an error as having occurred before promotion, so callers
// know the previous live version is intact.
func notPromoted(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w (live version unchanged): %w", ErrNotPromoted, err)
}

// CreateProjectRequest describes a new project (Flow G).
type CreateProjectRequest struct {
	Title       string
	Description string
	Slug        string
	Visibility  string // defaults to "public"

	// IndexHTML is the initial homepage. Required: a project without a
	// homepage is not useful.
	IndexHTML string

	// Files are additional non-index assets uploaded before the homepage.
	Files []Change
}

// CreateProjectResult records a completed project creation.
//
// RequestedSlug and Slug are both reported so a caller can see when WebSim
// granted a different slug than the one asked for.
type CreateProjectResult struct {
	OK             bool   `json:"ok"`
	Action         string `json:"action"`
	ProjectID      string `json:"project_id"`
	Slug           string `json:"slug"`
	RequestedSlug  string `json:"requested_slug,omitempty"`
	CurrentVersion int    `json:"current_version"`
	Verified       bool   `json:"verified"`
}

// CreateProject creates, populates, finalizes, and publishes a new project
// (Flow G).
func (c *Client) CreateProject(ctx context.Context, req CreateProjectRequest) (*CreateProjectResult, error) {
	if req.IndexHTML == "" {
		return nil, errors.New("create project: index.html content is required")
	}
	for _, f := range req.Files {
		clean, err := ValidatePath(f.Path)
		if err != nil {
			return nil, err
		}
		if IsIndexPath(clean) {
			return nil, fmt.Errorf("create project: pass the homepage as IndexHTML, not as a file")
		}
	}
	if req.Visibility == "" {
		req.Visibility = "public"
	}

	// Step 1: create the project and its initial draft revision.
	project, revision, err := c.createBlankProject(ctx, req.Visibility)
	if err != nil {
		return nil, err
	}
	projectID := project.ID
	_, version, err := revision.RequireIdentity()
	if err != nil {
		return nil, err
	}

	result := &CreateProjectResult{
		Action:    "create_project",
		ProjectID: projectID,
	}

	// Step 2: metadata. Slug acceptance is verified, not assumed.
	if req.Title != "" || req.Description != "" || req.Slug != "" {
		meta := ProjectMetadata{}
		if req.Title != "" {
			meta.Title = &req.Title
		}
		if req.Description != "" {
			meta.Description = &req.Description
		}
		if req.Slug != "" {
			meta.Slug = &req.Slug
			result.RequestedSlug = SanitizeSlug(req.Slug)
		}
		updated, err := c.UpdateProjectMetadata(ctx, projectID, meta)
		if err != nil {
			return nil, fmt.Errorf("project %s was created but metadata could not be set: %w", projectID, err)
		}
		result.Slug = updated.Slug
	}

	// Step 3: non-index files first.
	for _, f := range req.Files {
		if _, err := c.WriteAsset(ctx, projectID, version, f.Path, f.Content); err != nil {
			return nil, fmt.Errorf("project %s was created but %s could not be uploaded: %w", projectID, f.Path, err)
		}
	}

	// Step 4: the homepage.
	if _, err := c.WriteIndex(ctx, projectID, revision, req.IndexHTML, req.Description); err != nil {
		return nil, fmt.Errorf("project %s was created but the homepage could not be written: %w", projectID, err)
	}

	// Steps 5 through 7: finalize, promote, verify.
	if err := c.FinalizeRevision(ctx, projectID, version); err != nil {
		return nil, fmt.Errorf("project %s was created but its first revision could not be finalized: %w", projectID, err)
	}
	if err := c.setCurrentVersion(ctx, projectID, version); err != nil {
		return nil, fmt.Errorf("project %s was created but version %d could not be made current: %w", projectID, version, err)
	}
	if err := c.verifyCurrentVersion(ctx, projectID, version); err != nil {
		return nil, err
	}

	result.OK = true
	result.CurrentVersion = version
	result.Verified = true
	return result, nil
}
