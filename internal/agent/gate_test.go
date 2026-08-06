package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MetaMysteries8/webshim/internal/permission"
)

// TestGateFollowsTheModeMatrix is the safety property the permission modes
// exist for.
func TestGateFollowsTheModeMatrix(t *testing.T) {
	t.Parallel()

	type key struct {
		mode permission.Mode
		risk permission.Risk
	}
	wantAsked := map[key]bool{
		{permission.ModeManual, permission.RiskRead}:    true,
		{permission.ModeManual, permission.RiskEdit}:    true,
		{permission.ModeManual, permission.RiskCommand}: true,
		{permission.ModeNormal, permission.RiskRead}:    false,
		{permission.ModeNormal, permission.RiskEdit}:    false,
		{permission.ModeNormal, permission.RiskCommand}: true,
		{permission.ModeYOLO, permission.RiskRead}:      false,
		{permission.ModeYOLO, permission.RiskEdit}:      false,
		{permission.ModeYOLO, permission.RiskCommand}:   false,
	}

	for k, expectAsk := range wantAsked {
		asked := false
		gate := NewGate(k.mode, AskerFunc(func(context.Context, Request) (Decision, error) {
			asked = true
			return Decision{Approved: true}, nil
		}))

		if err := gate.Require(context.Background(), Request{Tool: "t", Risk: k.risk}); err != nil {
			t.Errorf("%s/%s: unexpected error %v", k.mode, k.risk, err)
		}
		if asked != expectAsk {
			t.Errorf("%s/%s: asked = %v, want %v", k.mode, k.risk, asked, expectAsk)
		}
	}
}

func TestGateDenialCarriesTheReasonBack(t *testing.T) {
	t.Parallel()

	gate := NewGate(permission.ModeNormal, AskerFunc(func(context.Context, Request) (Decision, error) {
		return Decision{Approved: false, Reason: "not until I review the copy"}, nil
	}))

	err := gate.Require(context.Background(), Request{Tool: "websim_publish", Risk: permission.RiskCommand})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("want ErrDenied, got %v", err)
	}
	// The model needs the reason to adapt rather than retry blindly.
	if !strings.Contains(err.Error(), "not until I review the copy") {
		t.Errorf("the refusal reason was lost: %v", err)
	}
}

// TestGateWithNoAskerDeniesRatherThanHangs covers the non-interactive case:
// there is nobody to approve, so a command must fail fast with a clear reason.
func TestGateWithNoAskerDeniesRatherThanHangs(t *testing.T) {
	t.Parallel()

	gate := NewGate(permission.ModeNormal, nil)

	// Reads and edits still work.
	for _, risk := range []permission.Risk{permission.RiskRead, permission.RiskEdit} {
		if err := gate.Require(context.Background(), Request{Tool: "t", Risk: risk}); err != nil {
			t.Errorf("%s should not need approval in normal mode: %v", risk, err)
		}
	}

	err := gate.Require(context.Background(), Request{Tool: "websim_publish", Risk: permission.RiskCommand})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("want ErrDenied, got %v", err)
	}
	if !strings.Contains(err.Error(), "not interactive") {
		t.Errorf("the error should explain why: %v", err)
	}
}

func TestGateAllowAlwaysSkipsSubsequentPrompts(t *testing.T) {
	t.Parallel()

	asks := 0
	gate := NewGate(permission.ModeManual, AskerFunc(func(context.Context, Request) (Decision, error) {
		asks++
		return Decision{Approved: true}, nil
	}))

	req := Request{Tool: "mirror_write", Risk: permission.RiskEdit}
	if err := gate.Require(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	gate.AllowAlways("mirror_write")
	for range 3 {
		if err := gate.Require(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	if asks != 1 {
		t.Errorf("asked %d times, want 1", asks)
	}

	// Allowing one tool must not allow another.
	if err := gate.Require(context.Background(),
		Request{Tool: "websim_publish", Risk: permission.RiskCommand}); err != nil {
		t.Fatal(err)
	}
	if asks != 2 {
		t.Errorf("a different tool should still prompt; asks = %d", asks)
	}
}

func TestGateModeCanChangeMidSession(t *testing.T) {
	t.Parallel()

	asks := 0
	gate := NewGate(permission.ModeManual, AskerFunc(func(context.Context, Request) (Decision, error) {
		asks++
		return Decision{Approved: true}, nil
	}))

	req := Request{Tool: "mirror_read", Risk: permission.RiskRead}
	_ = gate.Require(context.Background(), req)
	if asks != 1 {
		t.Fatalf("manual mode should have asked; asks = %d", asks)
	}

	gate.SetMode(permission.ModeYOLO)
	_ = gate.Require(context.Background(), req)
	if asks != 1 {
		t.Errorf("yolo mode should not ask; asks = %d", asks)
	}
	if gate.Mode() != permission.ModeYOLO {
		t.Errorf("Mode() = %q", gate.Mode())
	}
}

// TestGateRespectsCancellation: a person pressing escape while a prompt is open
// must abort the turn, not leave the tool blocked forever.
func TestGateRespectsCancellation(t *testing.T) {
	t.Parallel()

	gate := NewGate(permission.ModeManual, AskerFunc(func(ctx context.Context, _ Request) (Decision, error) {
		<-ctx.Done()
		return Decision{}, ctx.Err()
	}))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	err := gate.Require(ctx, Request{Tool: "websim_publish", Risk: permission.RiskCommand})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}

func TestGateAssignsRequestIDs(t *testing.T) {
	t.Parallel()

	var seen []int
	gate := NewGate(permission.ModeManual, AskerFunc(func(_ context.Context, r Request) (Decision, error) {
		seen = append(seen, r.ID)
		return Decision{Approved: true}, nil
	}))

	for range 3 {
		_ = gate.Require(context.Background(), Request{Tool: "t", Risk: permission.RiskRead})
	}
	if len(seen) != 3 || seen[0] == 0 || seen[0] == seen[1] || seen[1] == seen[2] {
		t.Errorf("request ids should be unique and non-zero: %v", seen)
	}
}

func TestAutoApproveAndDenyAll(t *testing.T) {
	t.Parallel()

	if err := NewGate(permission.ModeManual, AutoApprove()).
		Require(context.Background(), Request{Tool: "t", Risk: permission.RiskCommand}); err != nil {
		t.Errorf("AutoApprove should approve: %v", err)
	}

	err := NewGate(permission.ModeManual, DenyAll()).
		Require(context.Background(), Request{Tool: "websim_publish", Risk: permission.RiskCommand})
	if !errors.Is(err, ErrDenied) {
		t.Errorf("DenyAll should deny: %v", err)
	}
	if !strings.Contains(err.Error(), "websim_publish") {
		t.Errorf("the refusal should name the tool: %v", err)
	}
}
