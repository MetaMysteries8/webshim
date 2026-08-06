package websim

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Response normalization
// ---------------------------------------------------------------------------

func TestNormProjectAcceptsBothShapes(t *testing.T) {
	t.Parallel()

	wrapped := []byte(`{"project":{"id":"p1","slug":"s","current_version":12}}`)
	bare := []byte(`{"id":"p1","slug":"s","current_version":12}`)

	pw, err := normProject(wrapped)
	if err != nil {
		t.Fatalf("wrapped shape: %v", err)
	}
	pb, err := normProject(bare)
	if err != nil {
		t.Fatalf("bare shape: %v", err)
	}

	for name, p := range map[string]*Project{"wrapped": pw, "bare": pb} {
		if p.ID != "p1" || p.Slug != "s" {
			t.Errorf("%s: got id=%q slug=%q", name, p.ID, p.Slug)
		}
		v, err := p.RequireCurrentVersion()
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if v != 12 {
			t.Errorf("%s: current_version = %d, want 12", name, v)
		}
	}
}

func TestRequireCurrentVersionRejectsMissing(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		`{"id":"p1"}`,
		`{"id":"p1","current_version":null}`,
	} {
		p, err := normProject([]byte(body))
		if err != nil {
			t.Fatalf("normProject(%s): %v", body, err)
		}
		if _, err := p.RequireCurrentVersion(); !errors.Is(err, ErrUnexpectedShape) {
			t.Errorf("body %s: want ErrUnexpectedShape, got %v", body, err)
		}
	}
}

func TestNormProjectRejectsMissingID(t *testing.T) {
	t.Parallel()
	if _, err := normProject([]byte(`{"slug":"s"}`)); !errors.Is(err, ErrUnexpectedShape) {
		t.Errorf("want ErrUnexpectedShape, got %v", err)
	}
}

func TestNormalizersUnwrapEnvelopes(t *testing.T) {
	t.Parallel()

	revs, err := normRevisions([]byte(`{"revisions":{"data":[
		{"project_revision":{"id":"r12","version":12,"draft":false}},
		{"site":{"id":"s"}},
		{"project_revision":{"id":"r11","version":11,"draft":true}}]}}`))
	if err != nil {
		t.Fatalf("normRevisions: %v", err)
	}
	if len(revs) != 2 {
		t.Fatalf("got %d revisions, want 2 (the entry without project_revision is dropped)", len(revs))
	}

	comments, err := normComments([]byte(`{"comments":{"data":[{"comment":{"id":"c1","content":"hi"}}]}}`))
	if err != nil || len(comments) != 1 || comments[0].Content != "hi" {
		t.Fatalf("normComments: %v %+v", err, comments)
	}

	assets, err := normAssets([]byte(`{}`))
	if err != nil || assets == nil || len(assets) != 0 {
		t.Fatalf("normAssets on empty body: %v %+v", err, assets)
	}

	for _, tc := range []struct{ body, want string }{
		{`{"site":{"id":"s1"}}`, "s1"},
		{`{"id":"s2"}`, "s2"},
	} {
		got, err := normSiteID([]byte(tc.body))
		if err != nil || got != tc.want {
			t.Errorf("normSiteID(%s) = %q, %v; want %q", tc.body, got, err, tc.want)
		}
	}
	if _, err := normSiteID([]byte(`{"other":1}`)); !errors.Is(err, ErrUnexpectedShape) {
		t.Errorf("normSiteID with no id: want ErrUnexpectedShape, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Path safety and MIME
// ---------------------------------------------------------------------------

func TestValidatePathRejectsUnsafePaths(t *testing.T) {
	t.Parallel()

	bad := []string{
		"/etc/passwd",
		"C:/Windows/system32",
		`\\server\share`,
		"../secrets.txt",
		"assets/../../escape.txt",
		// The playbook rejects any component exactly equal to "..", even
		// one that would resolve back inside the project.
		"a/b/../c/file.js",
		"index (1).html",
		"index (12).HTML",
		"sub/index (3).html",
		"",
	}
	for _, p := range bad {
		if _, err := ValidatePath(p); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("ValidatePath(%q) = %v; want ErrUnsafePath", p, err)
		}
	}

	good := map[string]string{
		"index.html":      "index.html",
		"assets/logo.png": "assets/logo.png",
		"./style.css":     "style.css",
		"a/b/c/file.js":   "a/b/c/file.js",
	}
	for in, want := range good {
		got, err := ValidatePath(in)
		if err != nil {
			t.Errorf("ValidatePath(%q): unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ValidatePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEncodeContentPathEncodesSegmentsIndividually(t *testing.T) {
	t.Parallel()
	got := encodeContentPath("assets/my folder/a+b.png")
	want := "assets/my%20folder/a+b.png"
	if got != want {
		t.Errorf("encodeContentPath = %q, want %q", got, want)
	}
	if strings.Contains(got, "%2F") {
		t.Errorf("separators must not be encoded: %q", got)
	}
}

func TestDetectMIMESniffsUnknownExtensions(t *testing.T) {
	t.Parallel()

	if got := DetectMIME("style.css", nil); got != "text/css" {
		t.Errorf("known extension: got %q", got)
	}
	// A PNG signature behind an unknown extension must not be called text.
	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}
	if got := DetectMIME("mystery.bin", png); !strings.HasPrefix(got, "image/png") {
		t.Errorf("unknown extension with PNG bytes: got %q, want image/png", got)
	}
}

// ---------------------------------------------------------------------------
// Headers and encoding
// ---------------------------------------------------------------------------

func TestRequestsCarryStandardAuthHeaders(t *testing.T) {
	t.Parallel()

	f := newFakeServer(t)
	c := f.client(t)

	if _, err := c.GetSession(context.Background()); err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	calls := f.calls(http.MethodGet, "/session")
	if len(calls) != 1 {
		t.Fatalf("got %d session calls", len(calls))
	}
	got := calls[0]
	if got.Auth != "Bearer test-token-abcdefghijklmnop" {
		t.Errorf("Authorization = %q", got.Auth)
	}
	if got.Origin != "https://websim.com" {
		t.Errorf("Origin = %q, want https://websim.com", got.Origin)
	}
}

func TestCommentRequestsCarryProjectReferer(t *testing.T) {
	t.Parallel()

	f := newFakeServer(t)
	c := f.client(t)
	scope := CommentScope{ProjectID: f.projectID, Slug: "my-project"}
	ctx := context.Background()

	if _, err := c.ListComments(ctx, scope, 5); err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if _, err := c.PostComment(ctx, scope, "hello", ""); err != nil {
		t.Fatalf("PostComment: %v", err)
	}
	if err := c.DeleteComment(ctx, scope, "c1"); err != nil {
		t.Fatalf("DeleteComment: %v", err)
	}

	want := "https://websim.com/my-project"
	for _, r := range f.requests {
		if !strings.Contains(r.Path, "/comments") {
			continue
		}
		if r.Referer != want {
			t.Errorf("%s %s: Referer = %q, want %q", r.Method, r.Path, r.Referer, want)
		}
	}
}

func TestCommentScopeRequiresSlug(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	c := f.client(t)
	_, err := c.ListComments(context.Background(), CommentScope{ProjectID: "p"}, 5)
	if err == nil || !strings.Contains(err.Error(), "slug") {
		t.Errorf("want an error naming the slug, got %v", err)
	}
}

// TestAssetUploadMultipartShape asserts the exact wire format the API requires:
// a "contents" field holding a JSON metadata array, a "0" field holding the
// file, and a Content-Type whose boundary matches the body that was written.
func TestAssetUploadMultipartShape(t *testing.T) {
	t.Parallel()

	f := newFakeServer(t)
	c := f.client(t)
	draft, err := c.CreateDraft(context.Background(), f.projectID, 11)
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	version := *draft.Version

	if _, err := c.WriteAsset(context.Background(), f.projectID, version, "assets/logo.png", []byte("PNGDATA")); err != nil {
		t.Fatalf("WriteAsset: %v", err)
	}

	uploads := f.calls(http.MethodPost, "/assets")
	if len(uploads) != 1 {
		t.Fatalf("got %d upload calls, want 1", len(uploads))
	}
	up := uploads[0]

	mediaType, params, err := mime.ParseMediaType(up.ContentType)
	if err != nil {
		t.Fatalf("parsing Content-Type %q: %v", up.ContentType, err)
	}
	if mediaType != "multipart/form-data" {
		t.Errorf("media type = %q, want multipart/form-data", mediaType)
	}
	boundary := params["boundary"]
	if boundary == "" {
		t.Fatal("Content-Type carries no boundary")
	}
	// The boundary in the header must be the one actually used in the body.
	if !strings.Contains(string(up.Body), "--"+boundary) {
		t.Error("boundary in Content-Type does not appear in the body")
	}

	body := string(up.Body)
	if !strings.Contains(body, `name="contents"`) {
		t.Error(`missing the "contents" field`)
	}
	if !strings.Contains(body, `name="0"`) {
		t.Error(`missing the "0" file field`)
	}
	if !strings.Contains(body, `filename="assets/logo.png"`) {
		t.Error("file part does not carry the asset path as its filename")
	}
	if !strings.Contains(body, "image/png") {
		t.Error("file part does not carry the resolved MIME type")
	}
}

// TestAssetReplaceResolvesExistingID covers playbook rule 6: an overwrite must
// carry the existing asset's ID so it replaces rather than duplicates.
func TestAssetReplaceResolvesExistingID(t *testing.T) {
	t.Parallel()

	f := newFakeServer(t)
	c := f.client(t)
	ctx := context.Background()

	draft, err := c.CreateDraft(ctx, f.projectID, 11)
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	version := *draft.Version

	// style.css is inherited from the parent revision, so this is a replace.
	if _, err := c.WriteAsset(ctx, f.projectID, version, "style.css", []byte("body{}")); err != nil {
		t.Fatalf("replacing style.css: %v", err)
	}
	// A brand new path is a create.
	if _, err := c.WriteAsset(ctx, f.projectID, version, "new.css", []byte("a{}")); err != nil {
		t.Fatalf("creating new.css: %v", err)
	}

	uploads := f.calls(http.MethodPost, "/assets")
	if len(uploads) != 2 {
		t.Fatalf("got %d uploads, want 2", len(uploads))
	}
	if !strings.Contains(string(uploads[0].Body), `"existingAssetId":"asset_style"`) {
		t.Error("replace upload is missing existingAssetId")
	}
	if strings.Contains(string(uploads[1].Body), "existingAssetId") {
		t.Error("create upload must not carry existingAssetId")
	}
}

func TestWriteAssetRejectsIndexHTML(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	c := f.client(t)
	_, err := c.WriteAsset(context.Background(), f.projectID, 11, "index.html", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "POST /sites") {
		t.Errorf("want a refusal pointing at POST /sites, got %v", err)
	}
}

func TestDeleteAssetRefusesIndexAndMissingPaths(t *testing.T) {
	t.Parallel()
	f := newFakeServer(t)
	c := f.client(t)
	ctx := context.Background()

	if err := c.DeleteAsset(ctx, f.projectID, 11, "index.html"); err == nil {
		t.Error("deleting index.html must be refused")
	}
	err := c.DeleteAsset(ctx, f.projectID, 11, "does-not-exist.css")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting an absent path: want ErrNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Retry policy
// ---------------------------------------------------------------------------

func TestShouldRetryStatusByMethod(t *testing.T) {
	t.Parallel()

	cases := []struct {
		method string
		status int
		want   bool
	}{
		// Repeatable methods retry the full playbook list.
		{http.MethodGet, 408, true},
		{http.MethodGet, 409, true},
		{http.MethodGet, 425, true},
		{http.MethodGet, 429, true},
		{http.MethodGet, 500, true},
		{http.MethodGet, 503, true},
		{http.MethodPatch, 500, true},
		{http.MethodPatch, 409, true},

		// Never retried, for any method.
		{http.MethodGet, 400, false},
		{http.MethodGet, 401, false},
		{http.MethodGet, 403, false},
		{http.MethodGet, 404, false},
		{http.MethodPost, 401, false},
		{http.MethodPost, 403, false},
		{http.MethodPost, 404, false},

		// POST and DELETE may have committed before failing, so an
		// ambiguous status is surfaced instead of repeated.
		{http.MethodPost, 500, false},
		{http.MethodPost, 409, false},
		{http.MethodDelete, 500, false},
		// ...but a status that proves the server declined is safe.
		{http.MethodPost, 408, true},
		{http.MethodPost, 425, true},
		{http.MethodPost, 429, true},
	}
	for _, tc := range cases {
		if got := shouldRetryStatus(tc.method, tc.status); got != tc.want {
			t.Errorf("shouldRetryStatus(%s, %d) = %v, want %v", tc.method, tc.status, got, tc.want)
		}
	}
}

func TestBackoffFollowsPlaybookSequence(t *testing.T) {
	t.Parallel()

	p := DefaultRetryPolicy()
	p.Jitter = 0
	want := []time.Duration{750 * time.Millisecond, 1500 * time.Millisecond, 3000 * time.Millisecond}
	for i, w := range want {
		if got := p.delayFor(i + 1); got != w {
			t.Errorf("delayFor(%d) = %v, want %v", i+1, got, w)
		}
	}
	if got := p.delayFor(20); got != p.MaxDelay {
		t.Errorf("delayFor(20) = %v, want the %v cap", got, p.MaxDelay)
	}
}

func TestRetryAfterParsing(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := retryAfterDelay("3", now); got != 3*time.Second {
		t.Errorf("seconds form: %v", got)
	}
	future := now.Add(10 * time.Second).Format(http.TimeFormat)
	if got := retryAfterDelay(future, now); got <= 0 || got > 10*time.Second {
		t.Errorf("date form: %v", got)
	}
	if got := retryAfterDelay("", now); got != 0 {
		t.Errorf("absent header: %v", got)
	}
	if got := retryAfterDelay("garbage", now); got != 0 {
		t.Errorf("unparseable header: %v", got)
	}
}

func TestGetRetriesTransientStatuses(t *testing.T) {
	t.Parallel()

	f := newFakeServer(t)
	f.queueFailures("GET /project", 500, 503)
	c := f.client(t)

	p, err := c.GetProject(context.Background(), f.projectID)
	if err != nil {
		t.Fatalf("GetProject should have recovered: %v", err)
	}
	if p.ID != f.projectID {
		t.Errorf("project id = %q", p.ID)
	}
	if n := f.countCalls(http.MethodGet, "/projects/"+f.projectID); n != 3 {
		t.Errorf("made %d attempts, want 3 (two failures then success)", n)
	}
}

func TestUnauthorizedIsNotRetried(t *testing.T) {
	t.Parallel()

	f := newFakeServer(t)
	f.queueFailures("GET /project", 401, 401, 401, 401)
	c := f.client(t)

	_, err := c.GetProject(context.Background(), f.projectID)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
	if n := f.countCalls(http.MethodGet, "/projects/"+f.projectID); n != 1 {
		t.Errorf("made %d attempts, want exactly 1", n)
	}
}

func TestPostIsNotRetriedOnServerError(t *testing.T) {
	t.Parallel()

	f := newFakeServer(t)
	f.queueFailures("POST /revisions", 500)
	c := f.client(t)

	_, err := c.CreateDraft(context.Background(), f.projectID, 11)
	if err == nil {
		t.Fatal("expected an error")
	}
	// A repeat could create a second draft revision, so exactly one attempt.
	if n := f.countCalls(http.MethodPost, "/revisions"); n != 1 {
		t.Errorf("made %d POST attempts, want exactly 1", n)
	}
}

// ---------------------------------------------------------------------------
// Redaction
// ---------------------------------------------------------------------------

func TestErrorsAndLogsNeverCarryTheToken(t *testing.T) {
	t.Parallel()

	const secret = "test-token-abcdefghijklmnop"
	f := newFakeServer(t)
	f.queueFailures("GET /project", 403)
	c := f.client(t)

	_, err := c.GetProject(context.Background(), f.projectID)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error message leaked the token: %s", err)
	}

	// A token echoed back by the server must also be scrubbed.
	dirty := fmt.Sprintf("upstream said: Authorization: Bearer %s", secret)
	if clean := c.Sanitize(dirty); strings.Contains(clean, secret) {
		t.Errorf("Sanitize left the token in place: %s", clean)
	}
}

func TestTokenTypeRedactsItself(t *testing.T) {
	t.Parallel()

	tok := Token("super-secret-value-12345")
	if got := fmt.Sprintf("%v %s %#v", tok, tok, tok); strings.Contains(got, "super-secret") {
		t.Errorf("formatting leaked the token: %s", got)
	}
	if got, _ := tok.MarshalJSON(); strings.Contains(string(got), "super-secret") {
		t.Errorf("MarshalJSON leaked the token: %s", got)
	}
	if tok.Reveal() != "super-secret-value-12345" {
		t.Error("Reveal must return the real value")
	}
}

func TestMissingTokenIsAClearError(t *testing.T) {
	t.Parallel()

	f := newFakeServer(t)
	c, err := New(Options{BaseURL: f.srv.URL + "/api/v1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.GetProject(context.Background(), f.projectID)
	if !errors.Is(err, ErrNoToken) {
		t.Errorf("want ErrNoToken, got %v", err)
	}
}
