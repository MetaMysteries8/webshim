package mirror

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MetaMysteries8/webshim/internal/websim"
)

func openTestMirror(t *testing.T) *Mirror {
	t.Helper()
	m, err := Open(filepath.Join(t.TempDir(), "proj"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestWriteReadListDelete(t *testing.T) {
	t.Parallel()

	m := openTestMirror(t)
	if err := m.Write("index.html", []byte("<h1>hi</h1>")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Nested paths create their parents.
	if err := m.Write("assets/css/app.css", []byte("body{}")); err != nil {
		t.Fatalf("Write nested: %v", err)
	}

	got, err := m.Read("index.html")
	if err != nil || string(got) != "<h1>hi</h1>" {
		t.Fatalf("Read = %q, %v", got, err)
	}

	entries, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("List returned %d entries: %+v", len(entries), entries)
	}
	if entries[0].Path != "assets/css/app.css" || entries[1].Path != "index.html" {
		t.Errorf("entries are not sorted by path: %+v", entries)
	}

	if err := m.Delete("index.html"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := m.Read("index.html"); err == nil {
		t.Error("the file should be gone")
	}
}

// TestPathConfinement is the security property: nothing outside the mirror
// directory is reachable, and the mirror's own bookkeeping is not writable as
// project content.
func TestPathConfinement(t *testing.T) {
	t.Parallel()

	m := openTestMirror(t)
	bad := []string{
		"../escape.txt",
		"a/../../escape.txt",
		"/etc/passwd",
		"C:/Windows/system32/x",
		ManifestName,
		"index (1).html",
		"",
	}
	for _, p := range bad {
		if err := m.Write(p, []byte("x")); err == nil {
			t.Errorf("Write(%q) should have been refused", p)
		}
		if _, err := m.Read(p); err == nil {
			t.Errorf("Read(%q) should have been refused", p)
		}
		if err := m.Delete(p); err == nil {
			t.Errorf("Delete(%q) should have been refused", p)
		}
	}
}

// TestListExcludesDotfiles keeps the manifest, and anything like a stray .env,
// out of a publish.
func TestListExcludesDotfiles(t *testing.T) {
	t.Parallel()

	m := openTestMirror(t)
	if err := m.Write("index.html", []byte("x")); err != nil {
		t.Fatal(err)
	}
	// Write dotfiles directly, bypassing the guarded Write.
	for _, name := range []string{".env", ManifestName} {
		if err := os.WriteFile(filepath.Join(m.Dir, name), []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(m.Dir, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.Dir, ".hidden", "x.txt"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Path != "index.html" {
		t.Errorf("List should only see index.html, got %+v", entries)
	}
}

func TestDiffClassifiesChanges(t *testing.T) {
	t.Parallel()

	m := openTestMirror(t)
	if _, _, err := m.Diff(); !errors.Is(err, ErrNotSynced) {
		t.Errorf("an unsynced mirror should report ErrNotSynced, got %v", err)
	}

	// Establish a baseline.
	for path, body := range map[string]string{
		"index.html": "v1",
		"keep.css":   "same",
		"gone.js":    "bye",
	} {
		if err := m.Write(path, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	files, err := m.hashes()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteManifest(&Manifest{ProjectID: "p1", Version: 11, Files: files}); err != nil {
		t.Fatal(err)
	}

	// Now change things.
	if err := m.Write("index.html", []byte("v2")); err != nil { // modified
		t.Fatal(err)
	}
	if err := m.Write("new.txt", []byte("hello")); err != nil { // added
		t.Fatal(err)
	}
	if err := m.Delete("gone.js"); err != nil { // deleted
		t.Fatal(err)
	}

	diff, man, err := m.Diff()
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if man.Version != 11 {
		t.Errorf("manifest version = %d", man.Version)
	}
	assertPaths(t, "added", diff.Added, "new.txt")
	assertPaths(t, "modified", diff.Modified, "index.html")
	assertPaths(t, "deleted", diff.Deleted, "gone.js")
	assertPaths(t, "unchanged", diff.Unchanged, "keep.css")

	if diff.Empty() {
		t.Error("the diff should not be empty")
	}
	if got := diff.Summary(); !strings.Contains(got, "1 added") || !strings.Contains(got, "1 modified") {
		t.Errorf("Summary() = %q", got)
	}
}

func TestDiffEmptyWhenNothingChanged(t *testing.T) {
	t.Parallel()

	m := openTestMirror(t)
	if err := m.Write("index.html", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	files, _ := m.hashes()
	if err := m.WriteManifest(&Manifest{ProjectID: "p1", Version: 11, Files: files}); err != nil {
		t.Fatal(err)
	}

	diff, _, err := m.Diff()
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !diff.Empty() {
		t.Errorf("expected an empty diff, got %+v", diff)
	}
	if diff.Summary() != "no changes" {
		t.Errorf("Summary() = %q", diff.Summary())
	}
}

func TestCorruptManifestSuggestsResync(t *testing.T) {
	t.Parallel()

	m := openTestMirror(t)
	if err := os.WriteFile(filepath.Join(m.Dir, ManifestName), []byte("{ truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := m.ReadManifest()
	if err == nil || !strings.Contains(err.Error(), "re-sync") {
		t.Errorf("want an error suggesting a re-sync, got %v", err)
	}
}

// projectServer is a minimal stand-in that only answers what Plan needs.
func projectServer(t *testing.T, liveVersion int) *websim.Client {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"project":{"id":"p1","slug":"s","current_version":` +
			itoa(liveVersion) + `}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := websim.New(websim.Options{
		Token:   websim.Token("test-token-abcdefghijkl"),
		BaseURL: srv.URL + "/api/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestPlanRefusesStaleMirror is the local counterpart to the client's rebase
// guard: publishing a diff computed against an older parent would silently
// revert whatever changed in between.
func TestPlanRefusesStaleMirror(t *testing.T) {
	t.Parallel()

	m := openTestMirror(t)
	if err := m.Write("index.html", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	files, _ := m.hashes()
	if err := m.WriteManifest(&Manifest{ProjectID: "p1", Version: 11, Files: files}); err != nil {
		t.Fatal(err)
	}
	if err := m.Write("index.html", []byte("v2")); err != nil {
		t.Fatal(err)
	}

	// The live project has moved on to v12.
	client := projectServer(t, 12)
	_, err := m.Plan(context.Background(), client, "p1", "test")
	if !errors.Is(err, ErrStale) {
		t.Fatalf("want ErrStale, got %v", err)
	}
	if !strings.Contains(err.Error(), "sync again") {
		t.Errorf("the error should say what to do: %v", err)
	}
}

func TestPlanBuildsRequestFromDiff(t *testing.T) {
	t.Parallel()

	m := openTestMirror(t)
	for p, body := range map[string]string{"index.html": "v1", "old.css": "x"} {
		if err := m.Write(p, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := m.hashes()
	if err := m.WriteManifest(&Manifest{ProjectID: "p1", Version: 11, Files: files}); err != nil {
		t.Fatal(err)
	}

	if err := m.Write("index.html", []byte("v2")); err != nil { // modified
		t.Fatal(err)
	}
	if err := m.Write("app.js", []byte("console.log(1)")); err != nil { // added
		t.Fatal(err)
	}
	if err := m.Delete("old.css"); err != nil { // deleted
		t.Fatal(err)
	}

	plan, err := m.Plan(context.Background(), projectServer(t, 11), "p1", "a test edit")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.LiveVersion != 11 || plan.SyncedVersion != 11 {
		t.Errorf("versions = live %d, synced %d", plan.LiveVersion, plan.SyncedVersion)
	}
	if len(plan.Request.Writes) != 2 {
		t.Fatalf("expected 2 writes, got %+v", plan.Request.Writes)
	}
	// index.html is written first so the edit history reads sensibly.
	if plan.Request.Writes[0].Path != "index.html" {
		t.Errorf("index.html should be written first, got %q", plan.Request.Writes[0].Path)
	}
	if string(plan.Request.Writes[0].Content) != "v2" {
		t.Error("the write carries stale content")
	}
	assertPaths(t, "deletes", plan.Request.Deletes, "old.css")

	// Only changed files are uploaded; that is the whole point.
	for _, w := range plan.Request.Writes {
		if w.Path == "old.css" {
			t.Error("a deleted file was also queued as a write")
		}
	}
}

func TestPlanRefusesWhenNothingChanged(t *testing.T) {
	t.Parallel()

	m := openTestMirror(t)
	if err := m.Write("index.html", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	files, _ := m.hashes()
	if err := m.WriteManifest(&Manifest{ProjectID: "p1", Version: 11, Files: files}); err != nil {
		t.Fatal(err)
	}

	_, err := m.Plan(context.Background(), projectServer(t, 11), "p1", "test")
	if err == nil || !strings.Contains(err.Error(), "no local changes") {
		t.Errorf("want a clear refusal, got %v", err)
	}
}

func TestPlanRefusesWrongProject(t *testing.T) {
	t.Parallel()

	m := openTestMirror(t)
	if err := m.Write("index.html", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteManifest(&Manifest{ProjectID: "other", Version: 11, Files: map[string]string{}}); err != nil {
		t.Fatal(err)
	}

	_, err := m.Plan(context.Background(), projectServer(t, 11), "p1", "test")
	if err == nil || !strings.Contains(err.Error(), "synced from project other") {
		t.Errorf("want a mismatch error, got %v", err)
	}
}

func assertPaths(t *testing.T, label string, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", label, got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s = %v, want %v", label, got, want)
			return
		}
	}
}
