package websim

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// TestPublishHappyPathFollowsFlowB asserts both the outcome and the order of
// operations. The order is the safety property: branch before editing, finalize
// before promoting, verify after every mutation.
func TestPublishHappyPathFollowsFlowB(t *testing.T) {
	t.Parallel()

	f := newFakeServer(t)
	c := f.client(t)

	res, err := c.Publish(context.Background(), PublishRequest{
		ProjectID:   f.projectID,
		Writes:      []Change{{Path: "index.html", Content: []byte("<h1>hi</h1>")}},
		Description: "test edit",
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if !res.OK || !res.Verified {
		t.Errorf("result not verified: %+v", res)
	}
	if res.PreviousVersion != 11 || res.CurrentVersion != 12 {
		t.Errorf("versions = %d -> %d, want 11 -> 12", res.PreviousVersion, res.CurrentVersion)
	}
	if len(res.ChangedPaths) != 1 || res.ChangedPaths[0] != "index.html" {
		t.Errorf("changed paths = %v", res.ChangedPaths)
	}
	if f.currentVersion != 12 {
		t.Errorf("server current_version = %d, want 12", f.currentVersion)
	}

	// The site write must land before finalization, and finalization before
	// promotion.
	order := flowOrder(f)
	assertOrder(t, order,
		"POST /revisions", // branch a draft
		"POST /sites",     // write the homepage
		"PATCH /revision", // finalize
		"PATCH /project",  // promote
	)
}

// TestPublishWritesAssetsAndDeletes covers the mixed case.
func TestPublishWritesAssetsAndDeletes(t *testing.T) {
	t.Parallel()

	f := newFakeServer(t)
	c := f.client(t)

	res, err := c.Publish(context.Background(), PublishRequest{
		ProjectID: f.projectID,
		Writes: []Change{
			{Path: "index.html", Content: []byte("<h1>hi</h1>")},
			{Path: "assets/app.js", Content: []byte("console.log(1)")},
		},
		Deletes: []string{"style.css"},
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	rev := f.revisions[res.CurrentVersion]
	if _, ok := rev.Assets["assets/app.js"]; !ok {
		t.Error("assets/app.js was not written")
	}
	if _, ok := rev.Assets["style.css"]; ok {
		t.Error("style.css was not deleted")
	}
	if !rev.IndexSet {
		t.Error("index.html was not written")
	}
	if rev.Draft {
		t.Error("published revision is still a draft")
	}
}

// TestPublishStopsWhenFinalizationDoesNotTake is playbook step 7: if the
// revision is still a draft, do not promote. This is the guard that keeps rule
// 4 -- never set a draft revision as current -- from ever being violated.
func TestPublishStopsWhenFinalizationDoesNotTake(t *testing.T) {
	t.Parallel()

	f := newFakeServer(t)
	f.finalizeIsNoop = true
	c := f.client(t)

	_, err := c.Publish(context.Background(), PublishRequest{
		ProjectID: f.projectID,
		Writes:    []Change{{Path: "index.html", Content: []byte("x")}},
	})
	if err == nil {
		t.Fatal("expected publish to abort")
	}
	if !errors.Is(err, ErrNotPromoted) {
		t.Errorf("want ErrNotPromoted, got %v", err)
	}
	if !strings.Contains(err.Error(), "still a draft") {
		t.Errorf("error should name the cause: %v", err)
	}

	// The live version must be untouched, and promotion must never have been
	// attempted.
	if f.currentVersion != 11 {
		t.Errorf("live version changed to %d; it must stay at 11", f.currentVersion)
	}
	for _, r := range f.calls(http.MethodPatch, "/projects/"+f.projectID) {
		if strings.Contains(string(r.Body), "current_version") {
			t.Error("promotion was attempted despite the revision still being a draft")
		}
	}
}

// TestPublishRebaseGuard covers the concurrency rule: if another actor moves
// current_version while this edit is in flight, promoting would silently
// discard their work. The flow must stop instead.
func TestPublishRebaseGuard(t *testing.T) {
	t.Parallel()

	f := newFakeServer(t)
	c := f.client(t)

	// A second actor publishes version 99 in the window between our initial
	// read of current_version and our promotion.
	f.onAfterFinalize = func(f *fakeServer) {
		f.revisions[99] = &fakeRevision{ID: "rev_99", Version: 99, Draft: false, Assets: map[string]Asset{}}
		f.currentVersion = 99
	}

	_, err := c.Publish(context.Background(), PublishRequest{
		ProjectID: f.projectID,
		Writes:    []Change{{Path: "index.html", Content: []byte("x")}},
	})
	if err == nil {
		t.Fatal("expected publish to abort")
	}
	if !errors.Is(err, ErrConcurrentModification) {
		t.Errorf("want ErrConcurrentModification, got %v", err)
	}
	if f.currentVersion != 99 {
		t.Errorf("the other actor's version was overwritten: current_version = %d", f.currentVersion)
	}
	if !strings.Contains(err.Error(), "rebase") {
		t.Errorf("error should tell the caller to rebase: %v", err)
	}
}

// TestPublishTreatsLostPromotionResponseAsSuccess covers the recovery rule: if
// the PATCH was applied but its response was lost, re-reading state proves the
// change landed, so this is a success rather than a retry.
func TestPublishTreatsLostPromotionResponseAsSuccess(t *testing.T) {
	t.Parallel()

	f := newFakeServer(t)
	// Fail every attempt, but apply the change each time: the write lands,
	// the response never arrives.
	f.promoteFailures = 99
	f.promoteStatus = http.StatusBadGateway
	f.applyPromoteBeforeFailing = true
	c := f.client(t)

	res, err := c.Publish(context.Background(), PublishRequest{
		ProjectID: f.projectID,
		Writes:    []Change{{Path: "index.html", Content: []byte("x")}},
	})
	if err != nil {
		t.Fatalf("a lost response for an applied change should not fail: %v", err)
	}
	if res.CurrentVersion != 12 || f.currentVersion != 12 {
		t.Errorf("current version = %d (server %d), want 12", res.CurrentVersion, f.currentVersion)
	}
}

// TestPublishReportsUnappliedPromotion is the mirror case: the PATCH genuinely
// did not take, so the live version is unchanged and the caller is told so.
func TestPublishReportsUnappliedPromotion(t *testing.T) {
	t.Parallel()

	f := newFakeServer(t)
	f.promoteFailures = 99
	f.promoteStatus = http.StatusBadGateway
	f.applyPromoteBeforeFailing = false
	c := f.client(t)

	_, err := c.Publish(context.Background(), PublishRequest{
		ProjectID: f.projectID,
		Writes:    []Change{{Path: "index.html", Content: []byte("x")}},
	})
	if !errors.Is(err, ErrNotPromoted) {
		t.Fatalf("want ErrNotPromoted, got %v", err)
	}
	if f.currentVersion != 11 {
		t.Errorf("live version = %d, want 11", f.currentVersion)
	}
}

// TestPublishNeverBranchesFromADraftParent covers rule 3.
func TestPublishNeverBranchesFromADraftParent(t *testing.T) {
	t.Parallel()

	f := newFakeServer(t)
	f.revisions[11].Draft = true // the live revision is somehow a draft
	c := f.client(t)

	_, err := c.Publish(context.Background(), PublishRequest{
		ProjectID: f.projectID,
		Writes:    []Change{{Path: "index.html", Content: []byte("x")}},
	})
	if err == nil {
		t.Fatal("expected publish to refuse")
	}
	if !strings.Contains(err.Error(), "still a draft") {
		t.Errorf("error should name the draft parent: %v", err)
	}
	if f.countCalls(http.MethodPost, "/revisions") != 0 {
		t.Error("a draft must never be branched from")
	}
}

func TestPublishValidatesRequest(t *testing.T) {
	t.Parallel()

	f := newFakeServer(t)
	c := f.client(t)
	ctx := context.Background()

	cases := map[string]PublishRequest{
		"no project":       {Writes: []Change{{Path: "index.html", Content: []byte("x")}}},
		"nothing to do":    {ProjectID: f.projectID},
		"unsafe path":      {ProjectID: f.projectID, Writes: []Change{{Path: "../x.html"}}},
		"duplicate path":   {ProjectID: f.projectID, Writes: []Change{{Path: "a.css"}, {Path: "a.css"}}},
		"delete the index": {ProjectID: f.projectID, Deletes: []string{"index.html"}},
		"write and delete": {ProjectID: f.projectID, Writes: []Change{{Path: "a.css"}}, Deletes: []string{"a.css"}},
	}
	for name, req := range cases {
		if _, err := c.Publish(ctx, req); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
	// A rejected request must not have touched the server.
	if len(f.requests) != 0 {
		t.Errorf("validation failures made %d requests; they should be caught locally", len(f.requests))
	}
}

// TestPublishSerializesConcurrentFlows proves the per-project lock spans the
// whole flow. Without it, two publishes would branch from the same parent and
// one would clobber the other.
func TestPublishSerializesConcurrentFlows(t *testing.T) {
	t.Parallel()

	f := newFakeServer(t)
	c := f.client(t)

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = c.Publish(context.Background(), PublishRequest{
				ProjectID: f.projectID,
				Writes:    []Change{{Path: "index.html", Content: []byte("x")}},
			})
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("publish %d failed: %v", i, err)
		}
	}
	// Serialized flows each branch from the previous one, so the final live
	// version is the last of n sequential revisions.
	if want := 11 + n; f.currentVersion != want {
		t.Errorf("current_version = %d, want %d; flows were not serialized", f.currentVersion, want)
	}
}

// TestRollbackSelectsAnExistingRevision covers Flow F, including that it never
// deletes anything.
func TestRollbackSelectsAnExistingRevision(t *testing.T) {
	t.Parallel()

	f := newFakeServer(t)
	c := f.client(t)
	ctx := context.Background()

	if _, err := c.Publish(ctx, PublishRequest{
		ProjectID: f.projectID,
		Writes:    []Change{{Path: "index.html", Content: []byte("v12")}},
	}); err != nil {
		t.Fatalf("setup publish: %v", err)
	}

	res, err := c.Rollback(ctx, f.projectID, 11)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if res.PreRollbackVersion != 12 || res.CurrentVersion != 11 || !res.Verified {
		t.Errorf("unexpected result: %+v", res)
	}
	if f.currentVersion != 11 {
		t.Errorf("server current_version = %d, want 11", f.currentVersion)
	}
	// Rolling back selects; it must not remove the newer revision.
	if _, ok := f.revisions[12]; !ok {
		t.Error("rollback deleted the newer revision")
	}
}

func TestRollbackRefusesDrafts(t *testing.T) {
	t.Parallel()

	f := newFakeServer(t)
	c := f.client(t)
	f.revisions[20] = &fakeRevision{ID: "rev_20", Version: 20, Draft: true, Assets: map[string]Asset{}}

	_, err := c.Rollback(context.Background(), f.projectID, 20)
	if err == nil || !strings.Contains(err.Error(), "draft") {
		t.Errorf("want a refusal naming the draft, got %v", err)
	}
	if f.currentVersion != 11 {
		t.Errorf("live version changed to %d", f.currentVersion)
	}
}

// TestDryRunSuppressesMutations proves reads still work while writes stop at
// the first mutation.
func TestDryRunSuppressesMutations(t *testing.T) {
	t.Parallel()

	f := newFakeServer(t)
	c, err := New(Options{
		Token:   Token("test-token-abcdefghijklmnop"),
		BaseURL: f.srv.URL + "/api/v1",
		DryRun:  true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := c.GetProject(context.Background(), f.projectID); err != nil {
		t.Errorf("reads must still work in dry-run: %v", err)
	}
	_, err = c.Publish(context.Background(), PublishRequest{
		ProjectID: f.projectID,
		Writes:    []Change{{Path: "index.html", Content: []byte("x")}},
	})
	if !errors.Is(err, ErrDryRun) {
		t.Errorf("want ErrDryRun, got %v", err)
	}
	if f.countCalls(http.MethodPost, "/revisions") != 0 {
		t.Error("dry-run created a revision")
	}
}

func TestCreateProjectRunsTheFullLifecycle(t *testing.T) {
	t.Parallel()

	f := newFakeServer(t)
	c := f.client(t)

	res, err := c.CreateProject(context.Background(), CreateProjectRequest{
		Title:     "Smoke Test",
		Slug:      "Smoke Test!!",
		IndexHTML: "<h1>new</h1>",
		Files:     []Change{{Path: "app.js", Content: []byte("1")}},
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if !res.OK || !res.Verified {
		t.Errorf("result not verified: %+v", res)
	}
	if res.RequestedSlug != "smoke-test" {
		t.Errorf("slug was not sanitized: %q", res.RequestedSlug)
	}
}

func TestSanitizeSlug(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"My Fun Idea":     "my-fun-idea",
		"UPPER_case":      "upper-case",
		"  spaced  out  ": "spaced-out",
		"a--b":            "a-b",
		"!!!":             "",
		"trailing-":       "trailing",
	}
	for in, want := range cases {
		if got := SanitizeSlug(in); got != want {
			t.Errorf("SanitizeSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// flowOrder reduces the recorded requests to a comparable sequence of route
// keys.
func flowOrder(f *fakeServer) []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []string
	for _, r := range f.requests {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/revisions"):
			out = append(out, "POST /revisions")
		case r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/sites"):
			out = append(out, "POST /sites")
		case r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/assets"):
			out = append(out, "POST /assets")
		case r.Method == http.MethodPatch && strings.Contains(r.Path, "/revisions/"):
			out = append(out, "PATCH /revision")
		case r.Method == http.MethodPatch && strings.Contains(r.Path, "/projects/") && strings.Contains(string(r.Body), "current_version"):
			out = append(out, "PATCH /project")
		}
	}
	return out
}

// assertOrder checks that want appears as a subsequence of got.
func assertOrder(t *testing.T, got []string, want ...string) {
	t.Helper()
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Errorf("operation order %v does not contain the required sequence %v", got, want)
	}
}
