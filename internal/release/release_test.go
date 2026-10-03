package release

import (
	"runtime"
	"testing"
)

func TestCompare(t *testing.T) {
	tests := []struct {
		name   string
		local  string
		latest string
		want   Status
	}{
		{"identical tags", "v1.2.3", "v1.2.3", StatusUpToDate},
		{"v prefix only on one side", "1.2.3", "v1.2.3", StatusUpToDate},
		{"older patch", "v1.2.2", "v1.2.3", StatusOutdated},
		{"newer than published", "v1.3.0", "v1.2.3", StatusAhead},
		{"dev build", "dev", "v1.2.3", StatusDevelopment},
		{"git describe on the release commit", "v1.2.3-5-gabc1234", "v1.2.3", StatusDevelopment},
		// An unparseable local build still upgrades — that is how a machine
		// that installed a malformed release gets back onto a real one.
		{"unparseable local", "banana", "v1.2.3", StatusOutdated},
		// An unparseable latest must never read as "newer".
		{"unparseable latest", "v1.2.3", "definitely-no-such-tag-zzz", StatusUnknown},
		{"latest is a branch name", "v0.1.22", "main", StatusUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Compare(tt.local, tt.latest); got != tt.want {
				t.Errorf("Compare(%q, %q) = %v, want %v", tt.local, tt.latest, got, tt.want)
			}
		})
	}
}

func TestInstallCommandPinsThePlainVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("viti never runs the installer on Windows")
	}
	got := InstallCommand("v0.0.43")
	if want := UpgradeHint() + " -s -- --version v0.0.43"; got != want {
		t.Errorf("InstallCommand() = %q, want %q", got, want)
	}
}

// The tag ends up on a `bash -c` command line, so anything that is not a
// plain version is dropped rather than quoted.
func TestInstallCommandLeavesOutAnUnsafeTag(t *testing.T) {
	for _, tag := range []string{"", "latest", "v1.2.3; rm -rf ~", "v1.2.3 $(id)", "v1.2.3`id`"} {
		if got := InstallCommand(tag); got != UpgradeHint() {
			t.Errorf("InstallCommand(%q) = %q, want the unpinned hint", tag, got)
		}
	}
}
