package permission

import "testing"

// TestNeedsApprovalTable is the whole decision matrix. If this table ever
// changes, it should be a deliberate product decision, not a side effect.
func TestNeedsApprovalTable(t *testing.T) {
	t.Parallel()

	want := map[Mode]map[Risk]bool{
		ModeManual: {RiskRead: true, RiskEdit: true, RiskCommand: true},
		ModeNormal: {RiskRead: false, RiskEdit: false, RiskCommand: true},
		ModeYOLO:   {RiskRead: false, RiskEdit: false, RiskCommand: false},
	}

	for mode, byRisk := range want {
		for risk, expected := range byRisk {
			if got := NeedsApproval(mode, risk); got != expected {
				t.Errorf("NeedsApproval(%s, %s) = %v, want %v", mode, risk, got, expected)
			}
		}
	}
}

// TestUnknownModeFailsClosed guards against a typo in a config file quietly
// granting more autonomy than the operator intended.
func TestUnknownModeFailsClosed(t *testing.T) {
	t.Parallel()

	for _, r := range []Risk{RiskRead, RiskEdit, RiskCommand} {
		if !NeedsApproval(Mode("yol0"), r) {
			t.Errorf("an unrecognized mode must ask for %s", r)
		}
	}
}

func TestParseMode(t *testing.T) {
	t.Parallel()

	good := map[string]Mode{
		"manual": ModeManual,
		"normal": ModeNormal,
		"yolo":   ModeYOLO,
		"":       ModeNormal, // an unset config field means the default
	}
	for in, want := range good {
		got, err := ParseMode(in)
		if err != nil {
			t.Errorf("ParseMode(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseMode(%q) = %q, want %q", in, got, want)
		}
	}

	for _, bad := range []string{"YOLO", "auto", "off", "manual "} {
		if _, err := ParseMode(bad); err == nil {
			t.Errorf("ParseMode(%q) should have failed", bad)
		}
	}
}
