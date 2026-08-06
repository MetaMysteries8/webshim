package agent

import (
	"errors"
	"strings"
	"testing"

	"github.com/MetaMysteries8/webshim/internal/mirror"
	"github.com/MetaMysteries8/webshim/internal/permission"
	"github.com/MetaMysteries8/webshim/internal/websim"
)

// TestSystemPromptCarriesThePlatformAPI is the reason inproject.md exists: the
// model authors pages that call these, and it cannot invent the signatures.
func TestSystemPromptCarriesThePlatformAPI(t *testing.T) {
	t.Parallel()

	got, err := BuildSystemPrompt(ProjectContext{ProjectID: "p1", Alias: "demo"})
	if err != nil {
		t.Fatalf("BuildSystemPrompt: %v", err)
	}

	for _, want := range []string{
		"websim.chat.completions.create",
		"websim.imageGen",
		"websim.textToSpeech",
		"websim.getCurrentUser",
		"websim.getCurrentProject",
		"websim.postComment",
		"comment:created",
		"/api/v1/projects/${project.id}/comments",
		"5/min",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the system prompt is missing %q", want)
		}
	}
}

// TestSystemPromptCarriesTheSafetyRules: these are the rules that keep a
// credential out of a public page and stop duplicate homepages.
func TestSystemPromptCarriesTheSafetyRules(t *testing.T) {
	t.Parallel()

	got, err := BuildSystemPrompt(ProjectContext{ProjectID: "p1"})
	if err != nil {
		t.Fatalf("BuildSystemPrompt: %v", err)
	}

	for _, want := range []string{
		"Never put a credential in project content",
		"index (1).html",
		"public client-side code",
		"Never work around a refused tool call",
		"Never delete `index.html`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the system prompt is missing the rule %q", want)
		}
	}
}

func TestSystemPromptDescribesCurrentState(t *testing.T) {
	t.Parallel()

	got, err := BuildSystemPrompt(ProjectContext{
		Alias:       "demo",
		ProjectID:   "proj_abc",
		Slug:        "my-demo",
		Title:       "My Demo",
		LiveVersion: 12,
		MirrorDir:   "projects/demo",
		Mode:        permission.ModeNormal,
		MirrorFiles: []mirror.Entry{{Path: "index.html", Size: 400}},
		LiveAssets:  []websim.Asset{{Path: "style.css", Size: 120}},
		Diff:        &mirror.Diff{Modified: []string{"index.html"}},
	})
	if err != nil {
		t.Fatalf("BuildSystemPrompt: %v", err)
	}

	for _, want := range []string{
		"proj_abc", "my-demo", "My Demo", "v12", "projects/demo",
		"index.html", "style.css", "1 modified", "websim_publish",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the prompt should mention %q", want)
		}
	}
}

func TestSystemPromptTellsAnUnsyncedMirrorToSync(t *testing.T) {
	t.Parallel()

	got, err := BuildSystemPrompt(ProjectContext{ProjectID: "p1", Alias: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "websim_sync") {
		t.Errorf("an empty mirror should be told to sync: %s", got)
	}
}

func TestSystemPromptHandlesNoProject(t *testing.T) {
	t.Parallel()

	got, err := BuildSystemPrompt(ProjectContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "No project is selected") {
		t.Error("with no project, the prompt should say so")
	}
}

// TestExplainGivesEachFailureItsOwnRemedy: a generic error message makes a model
// retry the same failing call. A specific one lets it recover.
func TestExplainGivesEachFailureItsOwnRemedy(t *testing.T) {
	t.Parallel()

	d := &Deps{}
	cases := []struct {
		err  error
		want string
	}{
		{websim.ErrConcurrentModification, "websim_sync"},
		{websim.ErrNotPromoted, "previous revision is still current"},
		{websim.ErrUnauthorized, "websim-cli login"},
		{websim.ErrForbidden, "websim-cli login"},
		{websim.ErrUnsafePath, "Choose a different path"},
		{websim.ErrNotFound, "Re-read the current state"},
		{websim.ErrUnexpectedShape, "Stop and report"},
		{websim.ErrRateLimited, "Wait before trying again"},
		{ErrDenied, "Do not retry this"},
	}
	for _, tc := range cases {
		got := d.explain(tc.err)
		if !strings.Contains(got, tc.want) {
			t.Errorf("explain(%v) = %q, want it to contain %q", tc.err, got, tc.want)
		}
	}

	// Wrapped errors must still be recognised.
	wrapped := errors.New("publishing failed")
	if got := d.explain(wrapped); !strings.Contains(got, "publishing failed") {
		t.Errorf("an unclassified error should pass through: %q", got)
	}
}

func TestLooksLikeToolError(t *testing.T) {
	t.Parallel()

	errs := []string{
		"websim: unauthorized",
		"mirror: reading x: no such file",
		"the operator declined this action",
	}
	for _, s := range errs {
		if !looksLikeToolError(s) {
			t.Errorf("%q should be recognised as an error", s)
		}
	}
	ok := []string{"", `{"files":[]}`, "Published v11 -> v12"}
	for _, s := range ok {
		if looksLikeToolError(s) {
			t.Errorf("%q should not be recognised as an error", s)
		}
	}
}

func TestPreviewContentTrimsLongFiles(t *testing.T) {
	t.Parallel()

	short := "line1\nline2"
	if previewContent(short) != short {
		t.Error("short content should pass through unchanged")
	}

	long := strings.Repeat("x\n", 100)
	got := previewContent(long)
	if !strings.Contains(got, "more lines") {
		t.Errorf("long content should be trimmed with a note: %q", got)
	}
	if len(got) >= len(long) {
		t.Error("the preview is not shorter than the original")
	}
}

func TestTruncateForModel(t *testing.T) {
	t.Parallel()

	if got, truncated := truncateForModel("abc", 10); got != "abc" || truncated {
		t.Errorf("short input: %q %v", got, truncated)
	}
	if got, truncated := truncateForModel("abcdef", 3); got != "abc" || !truncated {
		t.Errorf("long input: %q %v", got, truncated)
	}
}
