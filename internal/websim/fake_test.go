package websim

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// recordedRequest is one request the fake server saw.
type recordedRequest struct {
	Method      string
	Path        string
	Query       string
	Auth        string
	Origin      string
	Referer     string
	ContentType string
	Body        []byte
}

// fakeRevision is the fake server's view of a revision.
type fakeRevision struct {
	ID       string
	Version  int
	Draft    bool
	Parent   int
	Assets   map[string]Asset
	IndexSet bool
}

// fakeServer is an in-memory stand-in for the WebSim API, faithful to the
// shapes the playbook documents. Tests drive it through a real *Client over a
// real HTTP connection, so header handling and multipart encoding are exercised
// end to end rather than mocked away.
type fakeServer struct {
	t  *testing.T
	mu sync.Mutex

	srv       *httptest.Server
	projectID string
	slug      string

	currentVersion int
	revisions      map[int]*fakeRevision
	nextVersion    int
	assetSeq       int

	requests []recordedRequest

	// Fault injection.

	// failNext maps "METHOD /path-suffix" to a queue of status codes to
	// return before behaving normally.
	failNext map[string][]int
	// finalizeIsNoop makes PATCH revision return 200 without clearing the
	// draft flag, simulating a finalize that silently fails.
	finalizeIsNoop bool
	// onAfterFinalize runs at the end of a successful finalize, which is the
	// window the rebase guard protects. Called with f.mu held, so the hook
	// must not lock.
	onAfterFinalize func(f *fakeServer)
	// promoteFailures is how many promote calls should fail before behaving
	// normally. Set it above RetryPolicy.MaxAttempts for a persistent
	// failure.
	promoteFailures int
	// promoteStatus is the status returned by a forced promote failure.
	promoteStatus int
	// applyPromoteBeforeFailing makes a forced promote failure still apply
	// the state change, simulating a write that landed while its response
	// was lost.
	applyPromoteBeforeFailing bool
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()

	f := &fakeServer{
		t:              t,
		projectID:      "proj_test",
		slug:           "test-project",
		currentVersion: 11,
		nextVersion:    12,
		revisions:      map[int]*fakeRevision{},
		failNext:       map[string][]int{},
	}
	// Seed a published parent revision holding one asset.
	f.revisions[11] = &fakeRevision{
		ID:      "rev_11",
		Version: 11,
		Draft:   false,
		Assets: map[string]Asset{
			"style.css": {ID: "asset_style", Path: "style.css", Size: 12, ContentType: "text/css"},
		},
		IndexSet: true,
	}

	mux := http.NewServeMux()
	f.routes(mux)
	f.srv = httptest.NewServer(f.record(mux))
	t.Cleanup(f.srv.Close)
	return f
}

// client builds a Client wired to this fake, with retry timings collapsed so
// tests stay fast.
func (f *fakeServer) client(t *testing.T) *Client {
	t.Helper()
	c, err := New(Options{
		Token:   Token("test-token-abcdefghijklmnop"),
		BaseURL: f.srv.URL + "/api/v1",
		ContentHostFn: func(projectID string) string {
			return f.srv.URL + "/content/" + projectID
		},
		Retry: RetryPolicy{
			MaxAttempts:    4,
			RequestTimeout: 5 * 1e9, // 5s
			BaseDelay:      1e6,     // 1ms
			MaxDelay:       5e6,     // 5ms
			Jitter:         0,
		},
	})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}
	return c
}

// record wraps a handler to capture every request.
func (f *fakeServer) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(body)))

		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{
			Method:      r.Method,
			Path:        r.URL.Path,
			Query:       r.URL.RawQuery,
			Auth:        r.Header.Get("Authorization"),
			Origin:      r.Header.Get("Origin"),
			Referer:     r.Header.Get("Referer"),
			ContentType: r.Header.Get("Content-Type"),
			Body:        body,
		})
		f.mu.Unlock()

		next.ServeHTTP(w, r)
	})
}

// takeFailure pops a queued fault for a route key, if any.
func (f *fakeServer) takeFailure(key string) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := f.failNext[key]
	if len(q) == 0 {
		return 0, false
	}
	status := q[0]
	f.failNext[key] = q[1:]
	return status, true
}

// queueFailures schedules status codes to return for a route key.
func (f *fakeServer) queueFailures(key string, statuses ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext[key] = append(f.failNext[key], statuses...)
}

// calls returns the recorded requests matching a method and path substring.
func (f *fakeServer) calls(method, pathContains string) []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedRequest
	for _, r := range f.requests {
		if r.Method == method && strings.Contains(r.Path, pathContains) {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeServer) countCalls(method, pathContains string) int {
	return len(f.calls(method, pathContains))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeServer) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/session", func(w http.ResponseWriter, r *http.Request) {
		if status, ok := f.takeFailure("GET /session"); ok {
			http.Error(w, `{"error":"injected"}`, status)
			return
		}
		writeJSON(w, 200, map[string]any{"user": map[string]any{"username": "test-user"}})
	})

	// Creating a project resets the fake onto that new project, so the rest
	// of the routes serve it. Each test gets its own fake, so this is safe.
	mux.HandleFunc("POST /api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		if status, ok := f.takeFailure("POST /projects"); ok {
			http.Error(w, `{"error":"injected"}`, status)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()

		f.projectID = "proj_created"
		f.slug = ""
		f.currentVersion = 0 // nothing is live until the first revision is promoted
		f.nextVersion = 2
		f.revisions = map[int]*fakeRevision{
			1: {ID: "rev_1", Version: 1, Draft: true, Assets: map[string]Asset{}},
		}
		writeJSON(w, 200, map[string]any{
			"project":          f.projectJSON(),
			"project_revision": f.revisionJSON(f.revisions[1]),
		})
	})

	mux.HandleFunc("GET /api/v1/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		if status, ok := f.takeFailure("GET /project"); ok {
			http.Error(w, `{"error":"injected"}`, status)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"project": f.projectJSON()})
	})

	mux.HandleFunc("PATCH /api/v1/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)

		if status, ok := f.takeFailure("PATCH /project"); ok {
			http.Error(w, `{"error":"injected"}`, status)
			return
		}

		f.mu.Lock()
		defer f.mu.Unlock()

		if v, ok := payload["current_version"]; ok {
			version := int(v.(float64))
			if f.promoteFailures > 0 {
				f.promoteFailures--
				if f.applyPromoteBeforeFailing {
					f.currentVersion = version
				}
				http.Error(w, `{"error":"injected promote failure"}`, f.promoteStatus)
				return
			}
			rev := f.revisions[version]
			if rev == nil {
				http.Error(w, `{"error":"no such revision"}`, 404)
				return
			}
			if rev.Draft {
				// The real API's behavior here is unknown; refusing is the
				// safe assumption and it lets a test prove the client never
				// asks.
				http.Error(w, `{"error":"cannot make a draft current"}`, 409)
				return
			}
			f.currentVersion = version
		}
		if s, ok := payload["slug"].(string); ok {
			f.slug = s
		}
		writeJSON(w, 200, map[string]any{"project": f.projectJSON()})
	})

	mux.HandleFunc("GET /api/v1/projects/{id}/revisions", func(w http.ResponseWriter, r *http.Request) {
		if status, ok := f.takeFailure("GET /revisions"); ok {
			http.Error(w, `{"error":"injected"}`, status)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()

		versions := make([]int, 0, len(f.revisions))
		for v := range f.revisions {
			versions = append(versions, v)
		}
		sort.Ints(versions)

		data := []any{}
		for _, v := range versions {
			rev := f.revisions[v]
			data = append(data, map[string]any{
				"project_revision": f.revisionJSON(rev),
				"site":             map[string]any{"id": "site_" + rev.ID, "title": "Test"},
			})
		}
		writeJSON(w, 200, map[string]any{"revisions": map[string]any{"data": data}})
	})

	mux.HandleFunc("POST /api/v1/projects/{id}/revisions", func(w http.ResponseWriter, r *http.Request) {
		if status, ok := f.takeFailure("POST /revisions"); ok {
			http.Error(w, `{"error":"injected"}`, status)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			ParentVersion int `json:"parent_version"`
		}
		_ = json.Unmarshal(body, &payload)

		f.mu.Lock()
		defer f.mu.Unlock()

		parent, ok := f.revisions[payload.ParentVersion]
		if !ok {
			http.Error(w, `{"error":"no such parent"}`, 404)
			return
		}

		version := f.nextVersion
		f.nextVersion++
		// A new draft inherits its parent's assets.
		assets := map[string]Asset{}
		for k, v := range parent.Assets {
			assets[k] = v
		}
		rev := &fakeRevision{
			ID:       fmt.Sprintf("rev_%d", version),
			Version:  version,
			Draft:    true,
			Parent:   payload.ParentVersion,
			Assets:   assets,
			IndexSet: parent.IndexSet,
		}
		f.revisions[version] = rev
		writeJSON(w, 200, map[string]any{"project_revision": f.revisionJSON(rev)})
	})

	mux.HandleFunc("PATCH /api/v1/projects/{id}/revisions/{version}", func(w http.ResponseWriter, r *http.Request) {
		if status, ok := f.takeFailure("PATCH /revision"); ok {
			http.Error(w, `{"error":"injected"}`, status)
			return
		}
		version, err := strconv.Atoi(r.PathValue("version"))
		if err != nil {
			http.Error(w, `{"error":"bad version"}`, 400)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)

		f.mu.Lock()
		defer f.mu.Unlock()

		rev, ok := f.revisions[version]
		if !ok {
			http.Error(w, `{"error":"no such revision"}`, 404)
			return
		}
		if d, ok := payload["draft"].(bool); ok && !f.finalizeIsNoop {
			rev.Draft = d
		}
		// Finalization is the last step before the rebase guard re-reads the
		// project, so this is where a test injects a competing actor.
		if f.onAfterFinalize != nil {
			hook := f.onAfterFinalize
			f.onAfterFinalize = nil
			hook(f)
		}
		writeJSON(w, 200, map[string]any{"project_revision": f.revisionJSON(rev)})
	})

	mux.HandleFunc("GET /api/v1/projects/{id}/revisions/{version}/assets", func(w http.ResponseWriter, r *http.Request) {
		if status, ok := f.takeFailure("GET /assets"); ok {
			http.Error(w, `{"error":"injected"}`, status)
			return
		}
		version, _ := strconv.Atoi(r.PathValue("version"))

		f.mu.Lock()
		defer f.mu.Unlock()

		rev, ok := f.revisions[version]
		if !ok {
			http.Error(w, `{"error":"no such revision"}`, 404)
			return
		}
		list := []Asset{}
		for _, a := range rev.Assets {
			list = append(list, a)
		}
		writeJSON(w, 200, map[string]any{"assets": list})
	})

	mux.HandleFunc("POST /api/v1/projects/{id}/revisions/{version}/assets", func(w http.ResponseWriter, r *http.Request) {
		if status, ok := f.takeFailure("POST /assets"); ok {
			http.Error(w, `{"error":"injected"}`, status)
			return
		}
		version, _ := strconv.Atoi(r.PathValue("version"))

		// The multipart body is parsed by hand rather than through
		// ParseMultipartForm because Go's form parser applies filepath.Base
		// to every filename (RFC 7578 section 4.2). That would silently turn
		// "assets/logo.png" into "logo.png" and hide a real client bug.
		// WebSim keys assets by their full project-relative path.
		upload, err := parseAssetUpload(r)
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, 400)
			return
		}

		f.mu.Lock()
		defer f.mu.Unlock()

		rev, ok := f.revisions[version]
		if !ok {
			http.Error(w, `{"error":"no such revision"}`, 404)
			return
		}

		id := upload.meta.ExistingAssetID
		if id == "" {
			f.assetSeq++
			id = fmt.Sprintf("asset_new_%d", f.assetSeq)
		}
		rev.Assets[upload.filename] = Asset{
			ID:          id,
			Path:        upload.filename,
			Size:        int64(len(upload.data)),
			ContentType: upload.contentType,
		}
		writeJSON(w, 200, map[string]any{"assets": []Asset{rev.Assets[upload.filename]}})
	})

	mux.HandleFunc("POST /api/v1/projects/{id}/revisions/{version}/edit-assets", func(w http.ResponseWriter, r *http.Request) {
		if status, ok := f.takeFailure("POST /edit-assets"); ok {
			http.Error(w, `{"error":"injected"}`, status)
			return
		}
		version, _ := strconv.Atoi(r.PathValue("version"))
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Operation struct {
				Type string `json:"type"`
				Path string `json:"path"`
			} `json:"operation"`
		}
		_ = json.Unmarshal(body, &payload)

		f.mu.Lock()
		defer f.mu.Unlock()

		rev, ok := f.revisions[version]
		if !ok {
			http.Error(w, `{"error":"no such revision"}`, 404)
			return
		}
		if payload.Operation.Type == "delete" {
			delete(rev.Assets, payload.Operation.Path)
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	mux.HandleFunc("GET /api/v1/projects/{id}/revisions/{version}/edit-history", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"edits": []any{
			map[string]any{"id": "edit_1", "created_at": "2026-01-01", "data": map[string]any{"type": "write", "path": "index.html"}},
		}})
	})

	mux.HandleFunc("POST /api/v1/sites", func(w http.ResponseWriter, r *http.Request) {
		if status, ok := f.takeFailure("POST /sites"); ok {
			http.Error(w, `{"error":"injected"}`, status)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			ProjectVersion    int    `json:"project_version"`
			ProjectRevisionID string `json:"project_revision_id"`
			Content           string `json:"content"`
		}
		_ = json.Unmarshal(body, &payload)

		f.mu.Lock()
		defer f.mu.Unlock()

		rev, ok := f.revisions[payload.ProjectVersion]
		if !ok {
			http.Error(w, `{"error":"no such revision"}`, 404)
			return
		}
		if rev.ID != payload.ProjectRevisionID {
			http.Error(w, `{"error":"revision id mismatch"}`, 400)
			return
		}
		rev.IndexSet = true
		writeJSON(w, 200, map[string]any{"site": map[string]any{"id": "site_" + rev.ID}})
	})

	// Comments.
	mux.HandleFunc("GET /api/v1/projects/{id}/comments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"comments": map[string]any{"data": []any{
			map[string]any{"comment": map[string]any{"id": "c1", "content": "hello", "created_at": "2026-01-01"}},
		}}})
	})
	mux.HandleFunc("GET /api/v1/projects/{id}/comments/{cid}/replies", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"comments": map[string]any{"data": []any{
			map[string]any{"comment": map[string]any{"id": "c2", "content": "reply", "parent_comment_id": r.PathValue("cid")}},
		}}})
	})
	mux.HandleFunc("POST /api/v1/projects/{id}/comments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"comment": map[string]any{"id": "c_new"}})
	})
	mux.HandleFunc("DELETE /api/v1/projects/{id}/comments/{cid}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	// Raw content host.
	mux.HandleFunc("/content/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(fakeIndexHTML)))
			w.WriteHeader(200)
			return
		}
		_, _ = w.Write([]byte(fakeIndexHTML))
	})
}

const fakeIndexHTML = "<!doctype html><html><body><h1>fake</h1></body></html>"

// assetUpload is one decoded multipart asset upload.
type assetUpload struct {
	meta        assetMetadata
	filename    string
	contentType string
	data        []byte
}

// parseAssetUpload decodes the "contents" and "0" parts by hand, preserving the
// full filename from Content-Disposition.
func parseAssetUpload(r *http.Request) (*assetUpload, error) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return nil, fmt.Errorf("bad Content-Type: %v", err)
	}
	boundary, ok := params["boundary"]
	if !ok {
		return nil, fmt.Errorf("Content-Type carries no boundary")
	}

	out := &assetUpload{}
	seenContents := false
	mr := multipart.NewReader(r.Body, boundary)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading part: %v", err)
		}
		data, err := io.ReadAll(part)
		if err != nil {
			return nil, fmt.Errorf("reading part body: %v", err)
		}

		switch part.FormName() {
		case "contents":
			var metas []assetMetadata
			if err := json.Unmarshal(data, &metas); err != nil {
				return nil, fmt.Errorf("contents is not a JSON array: %v", err)
			}
			if len(metas) != 1 {
				return nil, fmt.Errorf("contents has %d entries, want 1", len(metas))
			}
			out.meta = metas[0]
			seenContents = true
		case "0":
			_, dparams, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
			if err != nil {
				return nil, fmt.Errorf("bad Content-Disposition: %v", err)
			}
			out.filename = dparams["filename"]
			out.contentType = part.Header.Get("Content-Type")
			out.data = data
		}
		_ = part.Close()
	}

	if !seenContents {
		return nil, fmt.Errorf("missing contents field")
	}
	if out.filename == "" {
		return nil, fmt.Errorf("missing field 0 or its filename")
	}
	return out, nil
}

// projectJSON renders the project. Callers must hold f.mu.
func (f *fakeServer) projectJSON() map[string]any {
	return map[string]any{
		"id":              f.projectID,
		"slug":            f.slug,
		"title":           "Test Project",
		"visibility":      "public",
		"current_version": f.currentVersion,
	}
}

// revisionJSON renders a revision. Callers must hold f.mu.
func (f *fakeServer) revisionJSON(rev *fakeRevision) map[string]any {
	return map[string]any{
		"id":                      rev.ID,
		"project_id":              f.projectID,
		"version":                 rev.Version,
		"draft":                   rev.Draft,
		"parent_revision_version": rev.Parent,
		"created_at":              "2026-01-01T00:00:00Z",
	}
}
