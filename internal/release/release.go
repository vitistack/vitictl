// Package release queries GitHub for the latest published vitictl release
// and compares it against the locally installed version. Used by the
// `viti version --check` flag and the `viti upgrade` subcommand.
package release

import (
	"context"
	"fmt"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	pluginrelease "github.com/vitistack/vitictl/pkg/plugin/release"
)

// Repo is the GitHub owner/name that hosts vitictl releases.
const Repo = "vitistack/vitictl"

// Latest describes a single GitHub release entry.
type Latest = pluginrelease.Latest

// FetchLatest returns the newest published release of repo (expected to be
// "owner/name"). It shares the plugins' lookup, so it authenticates with the
// same token they use and survives GitHub's anonymous rate limit by falling
// back to the release page.
func FetchLatest(ctx context.Context, repo string) (*Latest, error) {
	return pluginrelease.FetchLatest(ctx, repo)
}

// Status classifies the result of comparing a local version against the
// latest release tag.
type Status int

const (
	// StatusUpToDate means local and latest point at the same release tag.
	StatusUpToDate Status = iota
	// StatusOutdated means latest is newer than the local build.
	StatusOutdated
	// StatusDevelopment means the local build is a dev or pre-release build
	// (e.g. "dev", or a git-describe tag like "v1.2.3-5-gabc1234") and we
	// cannot meaningfully say it is "out of date".
	StatusDevelopment
	// StatusAhead means the local build's semver is newer than the latest
	// published release — typical for unreleased main builds.
	StatusAhead
	// StatusUnknown means the latest release tag is not a version, so no
	// comparison is possible. Callers must not treat it as an upgrade.
	StatusUnknown
)

// Compare classifies the relationship between the locally installed
// version string and a GitHub release tag.
func Compare(local, latestTag string) Status {
	local = strings.TrimSpace(local)
	latestTag = strings.TrimSpace(latestTag)

	if local == "" || local == "dev" || local == "(devel)" {
		return StatusDevelopment
	}
	if local == latestTag || strings.TrimPrefix(local, "v") == strings.TrimPrefix(latestTag, "v") {
		return StatusUpToDate
	}

	lv, lok := parseSemver(local)
	rv, rok := parseSemver(latestTag)
	if !rok {
		// The release pointer is not a version. Never call that "newer":
		// upgrade installs on StatusOutdated, so a release published under
		// any other name would otherwise roll out to every machine that
		// asks. Seen for real on 2026-09-08.
		return StatusUnknown
	}
	if !lok {
		// A malformed local build still upgrades — that is the way back onto
		// a real release for a machine that installed one.
		return StatusOutdated
	}

	switch cmp := compareSemver(lv, rv); {
	case cmp < 0:
		return StatusOutdated
	case cmp > 0:
		return StatusAhead
	default:
		// Same X.Y.Z but strings differ — typically a git-describe suffix
		// like "v1.2.3-5-gabc1234" on the local build.
		if local != latestTag {
			return StatusDevelopment
		}
		return StatusUpToDate
	}
}

type semver struct {
	major, minor, patch int
}

// parseSemver extracts the leading X.Y.Z numeric components from a tag
// like "v1.2.3", "1.2.3", or "v1.2.3-5-gabc1234". Anything after the
// third numeric segment is ignored on purpose — we only compare the
// release portion.
func parseSemver(s string) (semver, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" {
		return semver{}, false
	}
	// Cut at the first non-numeric / non-dot character so suffixes like
	// "-5-gabc1234" or "-rc1" don't break parsing.
	end := len(s)
	for i, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			end = i
			break
		}
	}
	parts := strings.Split(s[:end], ".")
	if len(parts) < 3 {
		return semver{}, false
	}
	var v semver
	var err error
	if v.major, err = strconv.Atoi(parts[0]); err != nil {
		return semver{}, false
	}
	if v.minor, err = strconv.Atoi(parts[1]); err != nil {
		return semver{}, false
	}
	if v.patch, err = strconv.Atoi(parts[2]); err != nil {
		return semver{}, false
	}
	return v, true
}

func compareSemver(a, b semver) int {
	if a.major != b.major {
		return a.major - b.major
	}
	if a.minor != b.minor {
		return a.minor - b.minor
	}
	return a.patch - b.patch
}

// UpgradeHint returns a short, platform-appropriate command line the user
// can run to upgrade vitictl. The installer resolves the latest release
// on its own so no version needs to be baked into the command.
func UpgradeHint() string {
	switch runtime.GOOS {
	case "windows":
		return fmt.Sprintf(
			"iwr -useb https://raw.githubusercontent.com/%s/main/install.ps1 | iex",
			Repo,
		)
	default:
		return fmt.Sprintf(
			"curl -fsSL https://raw.githubusercontent.com/%s/main/install.sh | bash",
			Repo,
		)
	}
}

// installableTag matches the release tags InstallCommand will put on a shell
// command line.
var installableTag = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.+-]*$`)

// InstallCommand is UpgradeHint pinned to tag. Without the pin the installer
// looks the latest release up again, which is one more request against
// GitHub's API rate limit. A tag that is not a plain version is left out
// rather than spliced into a shell command line, and Windows keeps the plain
// hint because viti never runs the installer there.
func InstallCommand(tag string) string {
	if runtime.GOOS == "windows" || !installableTag.MatchString(tag) {
		return UpgradeHint()
	}
	return fmt.Sprintf("%s -s -- --version %s", UpgradeHint(), tag)
}

// ReleasesURL returns the human-readable releases page for Repo.
func ReleasesURL() string {
	return fmt.Sprintf("https://github.com/%s/releases", Repo)
}
