package cmd

import (
	"strings"
	"testing"

	"github.com/vitistack/vitictl/internal/release"
)

func TestSkipUpgradeLine(t *testing.T) {
	tests := []struct {
		name     string
		status   release.Status
		skip     bool
		contains string
	}{
		{"up to date is skipped", release.StatusUpToDate, true, "already up to date"},
		{"ahead is skipped", release.StatusAhead, true, "ahead of latest"},
		{"outdated installs", release.StatusOutdated, false, ""},
		{"development installs", release.StatusDevelopment, false, ""},
		// The one that matters: a latest tag that is not a version must be
		// refused, and the line must say why rather than look like success.
		{"unknown latest is refused", release.StatusUnknown, true, "not a version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, skip := skipUpgradeLine(tt.status, "nhn", "v0.1.22", "definitely-no-such-tag-zzz")
			if skip != tt.skip {
				t.Fatalf("skip = %v, want %v (line %q)", skip, tt.skip, line)
			}
			if tt.contains != "" && !strings.Contains(line, tt.contains) {
				t.Errorf("line %q should contain %q", line, tt.contains)
			}
			if !skip && line != "" {
				t.Errorf("an install should carry no skip line, got %q", line)
			}
		})
	}
}
