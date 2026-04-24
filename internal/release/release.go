// Package release queries GitHub for the latest published vitictl release
// and compares it against the locally installed version. Used by the
// `viti version --check` flag and the `viti upgrade` subcommand.
package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Repo is the GitHub owner/name that hosts vitictl releases.
const Repo = "vitistack/vitictl"

// DefaultTimeout bounds the GitHub API lookup so `viti version --check`
// cannot hang a user's terminal on a slow network.
const DefaultTimeout = 5 * time.Second

// Latest describes a single GitHub release entry.
type Latest struct {
	Tag  string `json:"tag_name"`
	Name string `json:"name"`
	URL  string `json:"html_url"`
	Body string `json:"body"`
}

// FetchLatest queries the GitHub releases API for the newest published
// release of repo (expected to be "owner/name"). A non-200 response or a
// network error is returned as-is so callers can surface a concise
// message.
func FetchLatest(ctx context.Context, repo string) (*Latest, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github API returned %s", resp.Status)
	}

	var out Latest
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding GitHub response: %w", err)
	}
	if out.Tag == "" {
		return nil, errors.New("github API response missing tag_name")
	}
	return &out, nil
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
	if !lok || !rok {
		// Fall back: if they aren't equal and we can't parse, treat as
		// outdated so the user at least sees the release pointer.
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

// ReleasesURL returns the human-readable releases page for Repo.
func ReleasesURL() string {
	return fmt.Sprintf("https://github.com/%s/releases", Repo)
}
