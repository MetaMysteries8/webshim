package websim

import (
	"encoding/json"
	"fmt"
)

// Token is a WebSim bearer token. Its String and MarshalJSON methods redact the
// value so that an accidental %v, %s, or json.Marshal in a log line, an error
// message, or a crash dump cannot leak the credential. Use Reveal only at the
// point where the token is written into an Authorization header.
//
// Playbook rules 1 and 14: never print, quote, summarize, upload, or commit a
// bearer token; redact Authorization headers and tokens from logs.
type Token string

// String implements fmt.Stringer with a redacted value.
func (t Token) String() string {
	if t == "" {
		return "[unset]"
	}
	return "[redacted]"
}

// GoString implements fmt.GoStringer so %#v is also safe.
func (t Token) GoString() string { return t.String() }

// MarshalJSON ensures a Token embedded in a struct cannot be serialized.
func (t Token) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// Reveal returns the raw token. Call this only when building an Authorization
// header.
func (t Token) Reveal() string { return string(t) }

// Project is a WebSim project.
//
// CurrentVersion is a pointer because the playbook requires proving that it is
// an integer before an edit flow may begin. A plain int cannot distinguish an
// absent or null field from a legitimate version 0.
type Project struct {
	ID             string `json:"id"`
	Slug           string `json:"slug,omitempty"`
	Title          string `json:"title,omitempty"`
	Description    string `json:"description,omitempty"`
	Visibility     string `json:"visibility,omitempty"`
	CommentsMode   string `json:"comments_mode,omitempty"`
	EnableChat     *bool  `json:"enable_chat,omitempty"`
	Posted         *bool  `json:"posted,omitempty"`
	CurrentVersion *int   `json:"current_version,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`

	// Raw retains the full server payload for fields this struct does not
	// model. The WebSim API is internal and unversioned, so preserving the
	// original response makes unexpected-shape reports actionable.
	Raw json.RawMessage `json:"-"`
}

// RequireCurrentVersion returns the live version, or an error when the field is
// absent or not an integer. Playbook: "Require project.current_version to be an
// integer before a normal edit flow."
func (p *Project) RequireCurrentVersion() (int, error) {
	if p == nil {
		return 0, fmt.Errorf("%w: project is nil", ErrUnexpectedShape)
	}
	if p.CurrentVersion == nil {
		return 0, fmt.Errorf("%w: project %q has no integer current_version", ErrUnexpectedShape, p.ID)
	}
	return *p.CurrentVersion, nil
}

// Revision is a project revision. Version is a pointer for the same reason as
// Project.CurrentVersion: a missing revision identity is a hard stop, never a
// defaulted zero.
type Revision struct {
	ID                    string `json:"id"`
	ProjectID             string `json:"project_id,omitempty"`
	Version               *int   `json:"version,omitempty"`
	Draft                 bool   `json:"draft"`
	ParentRevisionVersion *int   `json:"parent_revision_version,omitempty"`
	CreatedAt             string `json:"created_at,omitempty"`

	// Site is populated on list responses that include revisions.data[].site.
	Site *Site `json:"-"`

	Raw json.RawMessage `json:"-"`
}

// RequireIdentity returns the revision ID and version, or an error when either
// is missing. Playbook: "Do not fabricate defaults for required mutation
// identifiers."
func (r *Revision) RequireIdentity() (id string, version int, err error) {
	if r == nil {
		return "", 0, fmt.Errorf("%w: revision is nil", ErrUnexpectedShape)
	}
	if r.ID == "" {
		return "", 0, fmt.Errorf("%w: revision is missing id", ErrUnexpectedShape)
	}
	if r.Version == nil {
		return "", 0, fmt.Errorf("%w: revision %q is missing an integer version", ErrUnexpectedShape, r.ID)
	}
	return r.ID, *r.Version, nil
}

// Site is the site record attached to a revision.
type Site struct {
	ID     string      `json:"id,omitempty"`
	Title  string      `json:"title,omitempty"`
	Prompt *SitePrompt `json:"prompt,omitempty"`
}

// SitePrompt is the prompt metadata attached to a site.
type SitePrompt struct {
	Text string `json:"text,omitempty"`
}

// Asset is a non-index file in a revision.
type Asset struct {
	ID          string `json:"id"`
	Path        string `json:"path"`
	Size        int64  `json:"size,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

// Comment is a project comment.
//
// Only id, content, and created_at are modelled: the playbook documents the
// comments.data[].comment envelope but not the full comment shape. Raw keeps
// everything else so callers can inspect unmodelled fields without this package
// guessing at field names.
type Comment struct {
	ID              string `json:"id"`
	Content         string `json:"content,omitempty"`
	CreatedAt       string `json:"created_at,omitempty"`
	ParentCommentID string `json:"parent_comment_id,omitempty"`

	Raw json.RawMessage `json:"-"`
}

// Edit is one entry from a revision's edit history.
type Edit struct {
	ID        string   `json:"id"`
	CreatedAt string   `json:"created_at,omitempty"`
	NewPath   string   `json:"new_path,omitempty"`
	OldPath   string   `json:"old_path,omitempty"`
	Data      EditData `json:"data"`

	// By is retained raw because the playbook lists edit.by without
	// specifying its shape.
	By json.RawMessage `json:"by,omitempty"`
}

// EditData describes what an edit did.
type EditData struct {
	Type string `json:"type,omitempty"`
	Path string `json:"path,omitempty"`
}

// Session is the account behind the current bearer token.
type Session struct {
	User struct {
		ID       string `json:"id,omitempty"`
		Username string `json:"username,omitempty"`
	} `json:"user"`
}

// PublishResult is the non-secret operational record of a completed publish.
// It matches the playbook's agent output contract.
type PublishResult struct {
	OK              bool     `json:"ok"`
	Action          string   `json:"action"`
	ProjectID       string   `json:"project_id"`
	PreviousVersion int      `json:"previous_version"`
	CurrentVersion  int      `json:"current_version"`
	ChangedPaths    []string `json:"changed_paths"`
	Verified        bool     `json:"verified"`
}

// ---------------------------------------------------------------------------
// Response normalization
//
// The playbook documents two observed project shapes and nested envelopes for
// revisions and comments. These helpers implement exactly the defensive parsing
// the playbook sanctions -- and nothing more. An unrecognised shape is an error,
// not a zero value.
// ---------------------------------------------------------------------------

// normProject accepts either {"project": {...}} or a bare project object.
func normProject(b []byte) (*Project, error) {
	var envelope struct {
		Project *json.RawMessage `json:"project"`
	}
	// A decode failure here means the body is not a JSON object at all; fall
	// through so the bare-object attempt produces the clearer error.
	if err := json.Unmarshal(b, &envelope); err == nil && envelope.Project != nil {
		return decodeProject(*envelope.Project)
	}
	return decodeProject(b)
}

func decodeProject(b []byte) (*Project, error) {
	var p Project
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%w: decoding project: %v", ErrUnexpectedShape, err)
	}
	if p.ID == "" {
		return nil, fmt.Errorf("%w: project response has no id", ErrUnexpectedShape)
	}
	p.Raw = append(json.RawMessage(nil), b...)
	return &p, nil
}

// normRevisions extracts revisions.data[].project_revision, carrying along the
// sibling .site record when present.
func normRevisions(b []byte) ([]Revision, error) {
	var envelope struct {
		Revisions struct {
			Data []struct {
				ProjectRevision *json.RawMessage `json:"project_revision"`
				Site            *Site            `json:"site"`
			} `json:"data"`
		} `json:"revisions"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		return nil, fmt.Errorf("%w: decoding revisions: %v", ErrUnexpectedShape, err)
	}

	out := make([]Revision, 0, len(envelope.Revisions.Data))
	for _, row := range envelope.Revisions.Data {
		if row.ProjectRevision == nil {
			continue // playbook normalizer filters falsy entries
		}
		var r Revision
		if err := json.Unmarshal(*row.ProjectRevision, &r); err != nil {
			return nil, fmt.Errorf("%w: decoding project_revision: %v", ErrUnexpectedShape, err)
		}
		r.Raw = append(json.RawMessage(nil), *row.ProjectRevision...)
		r.Site = row.Site
		out = append(out, r)
	}
	return out, nil
}

// normSingleRevision extracts {"project_revision": {...}} from a create
// response.
func normSingleRevision(b []byte) (*Revision, error) {
	var envelope struct {
		ProjectRevision *json.RawMessage `json:"project_revision"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		return nil, fmt.Errorf("%w: decoding revision: %v", ErrUnexpectedShape, err)
	}
	if envelope.ProjectRevision == nil {
		return nil, fmt.Errorf("%w: response has no project_revision", ErrUnexpectedShape)
	}
	var r Revision
	if err := json.Unmarshal(*envelope.ProjectRevision, &r); err != nil {
		return nil, fmt.Errorf("%w: decoding project_revision: %v", ErrUnexpectedShape, err)
	}
	r.Raw = append(json.RawMessage(nil), *envelope.ProjectRevision...)
	return &r, nil
}

// normComments extracts comments.data[].comment.
func normComments(b []byte) ([]Comment, error) {
	var envelope struct {
		Comments struct {
			Data []struct {
				Comment *json.RawMessage `json:"comment"`
			} `json:"data"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		return nil, fmt.Errorf("%w: decoding comments: %v", ErrUnexpectedShape, err)
	}

	out := make([]Comment, 0, len(envelope.Comments.Data))
	for _, row := range envelope.Comments.Data {
		if row.Comment == nil {
			continue
		}
		var c Comment
		if err := json.Unmarshal(*row.Comment, &c); err != nil {
			return nil, fmt.Errorf("%w: decoding comment: %v", ErrUnexpectedShape, err)
		}
		c.Raw = append(json.RawMessage(nil), *row.Comment...)
		out = append(out, c)
	}
	return out, nil
}

// normAssets extracts body.assets, defaulting to an empty slice.
func normAssets(b []byte) ([]Asset, error) {
	var envelope struct {
		Assets []Asset `json:"assets"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		return nil, fmt.Errorf("%w: decoding assets: %v", ErrUnexpectedShape, err)
	}
	if envelope.Assets == nil {
		return []Asset{}, nil
	}
	return envelope.Assets, nil
}

// normSiteID accepts body.site.id or a top-level body.id.
func normSiteID(b []byte) (string, error) {
	var envelope struct {
		Site *struct {
			ID string `json:"id"`
		} `json:"site"`
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		return "", fmt.Errorf("%w: decoding site: %v", ErrUnexpectedShape, err)
	}
	if envelope.Site != nil && envelope.Site.ID != "" {
		return envelope.Site.ID, nil
	}
	if envelope.ID != "" {
		return envelope.ID, nil
	}
	return "", fmt.Errorf("%w: site response has neither site.id nor id", ErrUnexpectedShape)
}

// normEdits extracts body.edits.
func normEdits(b []byte) ([]Edit, error) {
	var envelope struct {
		Edits []Edit `json:"edits"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		return nil, fmt.Errorf("%w: decoding edit history: %v", ErrUnexpectedShape, err)
	}
	if envelope.Edits == nil {
		return []Edit{}, nil
	}
	return envelope.Edits, nil
}

// findRevision returns the revision with the given version, or nil.
func findRevision(revs []Revision, version int) *Revision {
	for i := range revs {
		if revs[i].Version != nil && *revs[i].Version == version {
			return &revs[i]
		}
	}
	return nil
}

// findAssetByPath returns the asset at an exact path, or nil. Matching is
// exact: the playbook forbids prefix, wildcard, or fuzzy matching for asset
// operations.
func findAssetByPath(assets []Asset, path string) *Asset {
	for i := range assets {
		if assets[i].Path == path {
			return &assets[i]
		}
	}
	return nil
}
