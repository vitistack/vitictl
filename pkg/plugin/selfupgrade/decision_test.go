package selfupgrade

import (
	"strings"
	"testing"

	"github.com/vitistack/vitictl/pkg/plugin/release"
)

// upgradeDecision is the switch behind `upgrade --run`: it decides whether
// the installer may run at all. Only a real, newer release (or a dev build
// switching to one) proceeds.
func TestUpgradeDecision(t *testing.T) {
	tests := []struct {
		name     string
		status   release.Status
		proceed  bool
		contains string
	}{
		{"up to date stops", release.StatusUpToDate, false, "nothing to do"},
		{"ahead stops", release.StatusAhead, false, "nothing to do"},
		{"development proceeds", release.StatusDevelopment, true, "development build"},
		{"outdated proceeds", release.StatusOutdated, true, "newer release"},
		{"latest that is not a version is refused", release.StatusUnknown, false, "not a version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, proceed := upgradeDecision(tt.status, "definitely-no-such-tag-zzz")
			if proceed != tt.proceed {
				t.Fatalf("proceed = %v, want %v (line %q)", proceed, tt.proceed, line)
			}
			if !strings.Contains(line, tt.contains) {
				t.Errorf("line %q should contain %q", line, tt.contains)
			}
		})
	}
}
