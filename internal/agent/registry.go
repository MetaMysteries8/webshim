package agent

import (
	"context"
	"fmt"
	"strings"

	"charm.land/fantasy"

	"github.com/MetaMysteries8/webshim/internal/permission"
	"github.com/MetaMysteries8/webshim/internal/websim"
)

// maxFileBytesForModel caps how much of a file is handed to the model in one
// tool result. Large assets would otherwise swallow the context window.
const maxFileBytesForModel = 60_000

// Tool input types. The `description` tags are what the model sees, so they
// carry the usage rules rather than repeating them in the tool description.

type emptyInput struct{}

type mirrorReadInput struct {
	Path string `json:"path" description:"Project-relative path, e.g. index.html or assets/app.css."`
}

type mirrorWriteInput struct {
	Path    string `json:"path" description:"Project-relative path. Use index.html for the homepage."`
	Content string `json:"content" description:"The complete new contents of the file. This replaces the file entirely."`
}

type mirrorDeleteInput struct {
	Path string `json:"path" description:"Project-relative path to remove from the local mirror."`
}

type publishInput struct {
	Description string `json:"description" description:"Short public summary of this change, shown in the project's history. No secrets."`
}

type rollbackInput struct {
	Version int `json:"version" description:"The published revision number to make live again."`
}

type readLiveInput struct {
	Path    string `json:"path" description:"Project-relative path to read from the live revision."`
	Version int    `json:"version,omitempty" description:"Optional revision number. Defaults to the live revision."`
}

type listAssetsInput struct {
	Version int `json:"version,omitempty" description:"Optional revision number. Defaults to the live revision."`
}

type commentsInput struct {
	Limit int `json:"limit,omitempty" description:"How many comments to return. Defaults to 20."`
}

type repliesInput struct {
	CommentID string `json:"comment_id" description:"The comment to list replies for."`
	Limit     int    `json:"limit,omitempty" description:"How many replies to return. Defaults to 20."`
}

type postCommentInput struct {
	Content         string `json:"content" description:"Markdown comment body."`
	ParentCommentID string `json:"parent_comment_id,omitempty" description:"Set this to reply to an existing comment."`
}

type deleteCommentInput struct {
	CommentID string `json:"comment_id" description:"The comment to delete."`
}

type updateMetaInput struct {
	Title       string `json:"title,omitempty" description:"New project title."`
	Description string `json:"description,omitempty" description:"New project description."`
	Slug        string `json:"slug,omitempty" description:"New URL slug. Lowercase letters, numbers, and hyphens only."`
}

// buildTools assembles the whole toolbelt.
//
// The set is deliberately narrow on the write side. There are no separate
// begin-draft, write-to-draft, and finalize tools: the playbook requires the
// per-project lock to span the entire read-branch-write-finalize-promote-verify
// sequence, and a model calling those steps as independent tools could not hold
// that lock across them. websim_publish is the single entry point, and it runs
// the whole transaction.
func buildTools(d *Deps) []toolDef {
	var defs []toolDef
	add := func(t toolDef) { defs = append(defs, t) }

	// -----------------------------------------------------------------
	// Reads
	// -----------------------------------------------------------------

	add(registerTool(d, "mirror_list",
		"List the files in the local working copy, with their sizes.",
		permission.RiskRead, nil,
		func(ctx context.Context, _ emptyInput) (any, error) {
			entries, err := d.Session.Mirror().List()
			if err != nil {
				return nil, err
			}
			if len(entries) == 0 {
				return "The local mirror is empty. Call websim_sync to pull the live revision.", nil
			}
			return map[string]any{"files": entries}, nil
		}))

	add(registerTool(d, "mirror_read",
		"Read a file from the local working copy. Use this before editing so you replace the right content.",
		permission.RiskRead, nil,
		func(ctx context.Context, in mirrorReadInput) (any, error) {
			data, err := d.Session.Mirror().Read(in.Path)
			if err != nil {
				return nil, err
			}
			text, truncated := truncateForModel(string(data), maxFileBytesForModel)
			out := map[string]any{"path": in.Path, "content": text, "bytes": len(data)}
			if truncated {
				out["truncated"] = true
				out["note"] = fmt.Sprintf(
					"Only the first %d of %d bytes are shown. Do not rewrite this file wholesale from "+
						"what you see here; you would lose the rest.", maxFileBytesForModel, len(data))
			}
			return out, nil
		}))

	add(registerTool(d, "mirror_diff",
		"Show what has changed in the local working copy since the last sync or publish.",
		permission.RiskRead, nil,
		func(ctx context.Context, _ emptyInput) (any, error) {
			diff, man, err := d.Session.Mirror().Diff()
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"synced_from_version": man.Version,
				"summary":             diff.Summary(),
				"added":               diff.Added,
				"modified":            diff.Modified,
				"deleted":             diff.Deleted,
			}, nil
		}))

	add(registerTool(d, "websim_get_project",
		"Read live project metadata: title, slug, visibility, and the current revision number.",
		permission.RiskRead, nil,
		func(ctx context.Context, _ emptyInput) (any, error) {
			project, err := d.Client.GetProject(ctx, d.Session.ProjectID())
			if err != nil {
				return nil, err
			}
			version, err := project.RequireCurrentVersion()
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"id": project.ID, "slug": project.Slug, "title": project.Title,
				"description": project.Description, "visibility": project.Visibility,
				"current_version": version,
			}, nil
		}))

	add(registerTool(d, "websim_list_revisions",
		"List the project's revisions, newest first, showing which is live and which are drafts.",
		permission.RiskRead, nil,
		func(ctx context.Context, _ emptyInput) (any, error) {
			revs, err := d.Client.ListRevisions(ctx, d.Session.ProjectID())
			if err != nil {
				return nil, err
			}
			live := d.Session.LiveVersion()
			out := make([]map[string]any, 0, len(revs))
			for i := len(revs) - 1; i >= 0; i-- {
				r := revs[i]
				if r.Version == nil {
					continue
				}
				out = append(out, map[string]any{
					"version": *r.Version, "draft": r.Draft, "live": *r.Version == live,
				})
			}
			return map[string]any{"revisions": out}, nil
		}))

	add(registerTool(d, "websim_list_assets",
		"List the files in a revision of the live project. index.html is managed separately and is not listed.",
		permission.RiskRead, nil,
		func(ctx context.Context, in listAssetsInput) (any, error) {
			version := in.Version
			if version == 0 {
				version = d.Session.LiveVersion()
			}
			assets, err := d.Client.ListAssets(ctx, d.Session.ProjectID(), version)
			if err != nil {
				return nil, err
			}
			return map[string]any{"version": version, "assets": assets}, nil
		}))

	add(registerTool(d, "websim_read_live_file",
		"Read a file as it exists in the published project, rather than from the local mirror.",
		permission.RiskRead, nil,
		func(ctx context.Context, in readLiveInput) (any, error) {
			version := in.Version
			if version == 0 {
				version = d.Session.LiveVersion()
			}
			data, err := d.Client.ReadFile(ctx, d.Session.ProjectID(), in.Path, version)
			if err != nil {
				return nil, err
			}
			text, truncated := truncateForModel(string(data), maxFileBytesForModel)
			return map[string]any{
				"path": in.Path, "version": version, "content": text,
				"bytes": len(data), "truncated": truncated,
			}, nil
		}))

	add(registerTool(d, "websim_edit_history",
		"Show the edit history of a revision.",
		permission.RiskRead, nil,
		func(ctx context.Context, in listAssetsInput) (any, error) {
			version := in.Version
			if version == 0 {
				version = d.Session.LiveVersion()
			}
			edits, err := d.Client.ListEditHistory(ctx, d.Session.ProjectID(), version)
			if err != nil {
				return nil, err
			}
			return map[string]any{"version": version, "edits": edits}, nil
		}))

	add(registerTool(d, "websim_list_comments",
		"List top-level comments on the project.",
		permission.RiskRead, nil,
		func(ctx context.Context, in commentsInput) (any, error) {
			comments, err := d.Client.ListComments(ctx, d.Session.CommentScope(), in.Limit)
			if err != nil {
				return nil, err
			}
			return map[string]any{"comments": comments}, nil
		}))

	add(registerTool(d, "websim_list_replies",
		"List replies to a comment.",
		permission.RiskRead, nil,
		func(ctx context.Context, in repliesInput) (any, error) {
			comments, err := d.Client.ListReplies(ctx, d.Session.CommentScope(), in.CommentID, in.Limit)
			if err != nil {
				return nil, err
			}
			return map[string]any{"replies": comments}, nil
		}))

	// -----------------------------------------------------------------
	// Edits: local only, nothing a visitor can see
	// -----------------------------------------------------------------

	add(registerTool(d, "mirror_write",
		"Create or replace a file in the local working copy. This does not publish anything. "+
			"Content must be complete: it replaces the whole file.",
		permission.RiskEdit,
		func(in mirrorWriteInput) (string, string) {
			return fmt.Sprintf("write %s (%d bytes) to the local mirror", in.Path, len(in.Content)),
				previewContent(in.Content)
		},
		func(ctx context.Context, in mirrorWriteInput) (any, error) {
			if err := d.Session.Mirror().Write(in.Path, []byte(in.Content)); err != nil {
				return nil, err
			}
			return map[string]any{
				"path": in.Path, "bytes": len(in.Content),
				"note": "Written locally. Call websim_publish to make it live.",
			}, nil
		}))

	add(registerTool(d, "mirror_delete",
		"Remove a file from the local working copy. It is removed from the live project only on the next publish.",
		permission.RiskEdit,
		func(in mirrorDeleteInput) (string, string) {
			return "delete " + in.Path + " from the local mirror", ""
		},
		func(ctx context.Context, in mirrorDeleteInput) (any, error) {
			if websim.IsIndexPath(in.Path) {
				return nil, fmt.Errorf("index.html is the project entrypoint and cannot be deleted; " +
					"edit it instead")
			}
			if err := d.Session.Mirror().Delete(in.Path); err != nil {
				return nil, err
			}
			return map[string]any{
				"path": in.Path,
				"note": "Removed locally. Call websim_publish to remove it from the live project.",
			}, nil
		}))

	// -----------------------------------------------------------------
	// Commands: these change what the public sees
	// -----------------------------------------------------------------

	add(registerTool(d, "websim_sync",
		"Download the live revision into the local working copy, replacing local files and "+
			"resetting the change tracking. Call this before editing, or after someone else publishes.",
		permission.RiskCommand,
		func(_ emptyInput) (string, string) {
			return "overwrite the local mirror with the live revision", ""
		},
		func(ctx context.Context, _ emptyInput) (any, error) {
			result, err := d.Session.Mirror().SyncDown(ctx, d.Client, d.Session.ProjectID())
			if err != nil {
				return nil, err
			}
			d.Session.Refresh(ctx)
			return result, nil
		}))

	add(registerTool(d, "websim_publish",
		"Publish the local working copy's changes to the live project. This branches a new "+
			"revision from the current one, uploads only what changed, finalizes it, makes it live, "+
			"and verifies the result. Returns the previous and new version numbers.",
		permission.RiskCommand,
		nil, // the summary needs a diff, so it is computed inside the handler
		func(ctx context.Context, in publishInput) (any, error) {
			return publishMirror(ctx, d, in.Description)
		}))

	add(registerTool(d, "websim_rollback",
		"Make an earlier published revision live again. This selects an existing revision; "+
			"it never deletes newer ones.",
		permission.RiskCommand,
		func(in rollbackInput) (string, string) {
			return fmt.Sprintf("make revision v%d live again", in.Version), ""
		},
		func(ctx context.Context, in rollbackInput) (any, error) {
			result, err := d.Client.Rollback(ctx, d.Session.ProjectID(), in.Version)
			if err != nil {
				return nil, err
			}
			d.Session.Refresh(ctx)
			return result, nil
		}))

	add(registerTool(d, "websim_update_project_metadata",
		"Change the project's title, description, or URL slug.",
		permission.RiskCommand,
		func(in updateMetaInput) (string, string) {
			var parts []string
			if in.Title != "" {
				parts = append(parts, "title to "+in.Title)
			}
			if in.Description != "" {
				parts = append(parts, "description")
			}
			if in.Slug != "" {
				parts = append(parts, "slug to "+websim.SanitizeSlug(in.Slug))
			}
			return "change the project's " + strings.Join(parts, ", "), ""
		},
		func(ctx context.Context, in updateMetaInput) (any, error) {
			meta := websim.ProjectMetadata{}
			if in.Title != "" {
				meta.Title = &in.Title
			}
			if in.Description != "" {
				meta.Description = &in.Description
			}
			if in.Slug != "" {
				meta.Slug = &in.Slug
			}
			project, err := d.Client.UpdateProjectMetadata(ctx, d.Session.ProjectID(), meta)
			if err != nil {
				return nil, err
			}
			out := map[string]any{"title": project.Title, "slug": project.Slug,
				"description": project.Description}
			// A slug request being accepted is not proof it was granted.
			if in.Slug != "" {
				wanted := websim.SanitizeSlug(in.Slug)
				if project.Slug != wanted {
					out["note"] = fmt.Sprintf(
						"The slug %q was requested but the project's slug is %q. "+
							"Tell the person; it may already be taken.", wanted, project.Slug)
				}
			}
			d.Session.Refresh(ctx)
			return out, nil
		}))

	add(registerTool(d, "websim_post_comment",
		"Post a comment on the project, or a reply to an existing comment. Comments are public.",
		permission.RiskCommand,
		func(in postCommentInput) (string, string) {
			what := "post a public comment"
			if in.ParentCommentID != "" {
				what = "post a public reply"
			}
			return what, in.Content
		},
		func(ctx context.Context, in postCommentInput) (any, error) {
			if _, err := d.Client.PostComment(ctx, d.Session.CommentScope(),
				in.Content, in.ParentCommentID); err != nil {
				return nil, err
			}
			return map[string]any{
				"posted": true,
				"note": "If you are unsure whether this succeeded, list the comments to check. " +
					"Do not post again without checking.",
			}, nil
		}))

	add(registerTool(d, "websim_delete_comment",
		"Delete a comment.",
		permission.RiskCommand,
		func(in deleteCommentInput) (string, string) {
			return "delete comment " + in.CommentID, ""
		},
		func(ctx context.Context, in deleteCommentInput) (any, error) {
			if err := d.Client.DeleteComment(ctx, d.Session.CommentScope(), in.CommentID); err != nil {
				return nil, err
			}
			return map[string]any{"deleted": in.CommentID}, nil
		}))

	return defs
}

// publishMirror runs the mirror publish flow, asking for approval with a diff
// the operator can actually evaluate.
//
// The approval prompt is built here rather than in registerTool's summarize hook
// because it needs the computed diff, which requires I/O.
func publishMirror(ctx context.Context, d *Deps, description string) (any, error) {
	m := d.Session.Mirror()
	projectID := d.Session.ProjectID()

	if description == "" {
		description = "webshim edit"
	}

	plan, err := m.Plan(ctx, d.Client, projectID, description)
	if err != nil {
		return nil, err
	}

	var detail strings.Builder
	detail.WriteString(describePaths("  +", plan.Diff.Added))
	detail.WriteString(describePaths("  ~", plan.Diff.Modified))
	detail.WriteString(describePaths("  -", plan.Diff.Deleted))

	if err := d.Gate.Require(ctx, Request{
		Tool: "websim_publish",
		Risk: permission.RiskCommand,
		Summary: fmt.Sprintf("publish to %s: %s (v%d -> new revision)",
			d.Session.Alias(), plan.Diff.Summary(), plan.LiveVersion),
		Detail: detail.String(),
	}); err != nil {
		return nil, err
	}

	result, err := m.Publish(ctx, d.Client, plan)
	if err != nil {
		return nil, err
	}
	d.Session.Refresh(ctx)
	return result, nil
}

// previewContent trims file content for an approval prompt. The operator needs
// enough to judge the change, not the whole file.
func previewContent(s string) string {
	const maxLines = 40
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	return strings.Join(lines[:maxLines], "\n") +
		fmt.Sprintf("\n... (%d more lines)", len(lines)-maxLines)
}

// fantasyTools extracts the Fantasy tools from a toolbelt.
func fantasyTools(defs []toolDef) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Tool)
	}
	return out
}

// riskByTool indexes risk classes by tool name, for the UI to label a pending
// approval.
func riskByTool(defs []toolDef) map[string]permission.Risk {
	out := make(map[string]permission.Risk, len(defs))
	for _, d := range defs {
		out[d.Name] = d.Risk
	}
	return out
}
