// Package pluginmgr implements installation and upgrade of viti plugins
// listed in the curated plugins.yaml index hosted alongside vitictl.
//
// The index maps short plugin names (e.g. "gui", "kubevirt") to GitHub
// repos and optional template overrides. Plugin authors who follow the
// canonical release layout — assets named
//
//	viti-<name>-<version>-<os>-<arch>.tar.gz
//	viti-<name>-<version>-SHA256SUMS
//
// with the binary nested inside a matching directory — need no overrides.
// Plugins released alongside vitictl itself (currently just viti-gui)
// override checksumsAsset because the checksums file is shared with the
// main viti binary.
package pluginmgr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/vitistack/vitictl/internal/settings"
)

// DefaultIndexURL is where `viti plugin` fetches the curated plugin
// catalog. Users can override via the VITICTL_PLUGINS_INDEX environment
// variable — useful for testing or for running a private index.
const DefaultIndexURL = "https://raw.githubusercontent.com/vitistack/vitictl/main/plugins.yaml"

// IndexEnvVar is the environment variable consulted before falling back
// to DefaultIndexURL.
const IndexEnvVar = "VITICTL_PLUGINS_INDEX"

// resolveIndexURL honors VITICTL_PLUGINS_INDEX if set and non-empty.
func resolveIndexURL() string {
	if v := strings.TrimSpace(os.Getenv(IndexEnvVar)); v != "" {
		return v
	}
	return DefaultIndexURL
}

// fetchTimeout bounds the index download so a slow network doesn't hang
// `viti plugin install`.
const fetchTimeout = 5 * time.Second

// Index is the structure serialised in plugins.yaml.
type Index struct {
	Plugins []Entry `json:"plugins" yaml:"plugins"`
}

// Entry is one row in plugins.yaml. All override fields are optional;
// their zero values fall back to conventional templates derived from
// Name and Repo.
type Entry struct {
	// Private keeps an entry out of `viti plugin list --available` unless
	// --all is passed. It controls advertising, not access: the plugin still
	// installs normally for anyone who can read its repository.
	Private bool `json:"private,omitempty" yaml:"private,omitempty"`

	Name                string `json:"name"                          yaml:"name"`
	Repo                string `json:"repo"                          yaml:"repo"`
	Description         string `json:"description,omitempty"         yaml:"description,omitempty"`
	ArchiveAsset        string `json:"archiveAsset,omitempty"        yaml:"archiveAsset,omitempty"`
	ArchiveInnerPath    string `json:"archiveInnerPath,omitempty"    yaml:"archiveInnerPath,omitempty"`
	ChecksumsAsset      string `json:"checksumsAsset,omitempty"      yaml:"checksumsAsset,omitempty"`
	CosignIdentityRegex string `json:"cosignIdentityRegex,omitempty" yaml:"cosignIdentityRegex,omitempty"`
}

// extraIndexes returns additional index URLs configured by the user. It is a
// variable so tests can supply their own.
var extraIndexes = settings.PluginIndexes

// IndexSources lists every index to read, in precedence order: the default
// public index (or whatever VITICTL_PLUGINS_INDEX replaces it with) first,
// then any configured in ~/.vitistack/ctl.config.yaml.
func IndexSources() []string {
	sources := []string{resolveIndexURL()}
	for _, u := range extraIndexes() {
		if u = strings.TrimSpace(u); u != "" {
			sources = append(sources, u)
		}
	}
	return sources
}

// FetchIndex reads every configured index and merges them into one.
//
// Later sources win on a name clash: a team's own index is more specific than
// the shared public one, so it can override an entry. A source that cannot be
// read is reported and skipped, so one unreachable internal index does not
// block installing everything else; an error is returned only when nothing
// could be read at all.
func FetchIndex(ctx context.Context) (*Index, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, fetchTimeout)
		defer cancel()
	}

	var (
		merged   Index
		position = map[string]int{}
		failures []error
		read     int
	)
	for _, src := range IndexSources() {
		idx, err := fetchOneIndex(ctx, src)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", src, err))
			_, _ = fmt.Fprintf(warnOut, "==> ⚠️  skipping plugin index %s: %v\n", src, err)
			continue
		}
		read++
		for _, e := range idx.Plugins {
			if at, dup := position[e.Name]; dup {
				merged.Plugins[at] = e // later source wins
				continue
			}
			position[e.Name] = len(merged.Plugins)
			merged.Plugins = append(merged.Plugins, e)
		}
	}
	if read == 0 {
		if len(failures) == 1 {
			return nil, failures[0]
		}
		return nil, fmt.Errorf("no plugin index could be read: %v", errors.Join(failures...))
	}
	return &merged, nil
}

// fetchOneIndex downloads and validates a single index source.
func fetchOneIndex(ctx context.Context, src string) (*Index, error) {
	body, err := fetchIndexBody(ctx, src)
	if err != nil {
		return nil, err
	}
	var idx Index
	if err := yaml.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("parsing plugins.yaml: %w", err)
	}
	for i := range idx.Plugins {
		if err := idx.Plugins[i].validate(); err != nil {
			return nil, fmt.Errorf("plugins[%d]: %w", i, err)
		}
	}
	return &idx, nil
}

// fetchIndexBody downloads the index, authenticating when it is hosted on
// GitHub and a token is available.
//
// Any failure of the authenticated attempt is retried anonymously. A stale
// token makes raw.githubusercontent.com answer 404 rather than 401 for a
// public file, so a narrower "only retry on 401" rule would still let a
// leftover token break every plugin command.
func fetchIndexBody(ctx context.Context, url string) ([]byte, error) {
	authorize := shouldAuthenticate(url) && token() != ""
	body, err := getIndex(ctx, url, authorize)
	if err == nil {
		return body, nil
	}
	if !authorize {
		return nil, err
	}
	body, retryErr := getIndex(ctx, url, false)
	if retryErr != nil {
		// Report the authenticated attempt: it is the more specific failure.
		return nil, err
	}
	_, _ = fmt.Fprintf(warnOut, "==> ⚠️  could not read the plugin index with the configured token (%v); continuing without authentication\n", err)
	return body, nil
}

func getIndex(ctx context.Context, url string, authorize bool) ([]byte, error) {
	req, err := newAPIRequest(ctx, url, "*/*", authorize)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("index returned %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// Listable returns the entries to show in a listing. Private entries are
// included only when all is true.
func (i *Index) Listable(all bool) *Index {
	out := &Index{}
	for _, e := range i.Plugins {
		if e.Private && !all {
			continue
		}
		out.Plugins = append(out.Plugins, e)
	}
	return out
}

// Find returns the entry with the given name, case-sensitively.
func (i *Index) Find(name string) (*Entry, bool) {
	for idx := range i.Plugins {
		if i.Plugins[idx].Name == name {
			return &i.Plugins[idx], true
		}
	}
	return nil, false
}

func (e *Entry) validate() error {
	if e.Name == "" {
		return errors.New("plugin entry missing name")
	}
	if e.Repo == "" {
		return errors.New("plugin entry missing repo")
	}
	if !strings.Contains(e.Repo, "/") {
		return fmt.Errorf("plugin %q: repo %q must be owner/name", e.Name, e.Repo)
	}
	return nil
}

// resolve fills in the template placeholders for name/repo/version/os/arch
// on the given template, applying default templates when the entry-level
// override is empty.
func (e *Entry) resolve(tmpl, defaultTmpl, version, goos, goarch string) string {
	if tmpl == "" {
		tmpl = defaultTmpl
	}
	r := strings.NewReplacer(
		"{name}", e.Name,
		"{repo}", e.Repo,
		"{version}", version,
		"{os}", goos,
		"{arch}", goarch,
	)
	return r.Replace(tmpl)
}

// ArchiveName returns the release asset filename for the given
// platform+version, applying the entry's override or the default.
func (e *Entry) ArchiveName(version, goos, goarch string) string {
	return e.resolve(
		e.ArchiveAsset,
		"viti-{name}-{version}-{os}-{arch}.tar.gz",
		version, goos, goarch,
	)
}

// InnerBinaryPath returns the path inside the archive where the plugin
// binary lives (relative to the archive root).
func (e *Entry) InnerBinaryPath(version, goos, goarch string) string {
	return e.resolve(
		e.ArchiveInnerPath,
		"viti-{name}-{version}-{os}-{arch}/viti-{name}",
		version, goos, goarch,
	)
}

// ChecksumsName returns the SHA256SUMS filename for the given version.
func (e *Entry) ChecksumsName(version string) string {
	return e.resolve(
		e.ChecksumsAsset,
		"viti-{name}-{version}-SHA256SUMS",
		version, "", "",
	)
}

// CosignIdentity returns the OIDC identity regex cosign should accept
// when verifying release signatures.
func (e *Entry) CosignIdentity() string {
	if e.CosignIdentityRegex != "" {
		return e.CosignIdentityRegex
	}
	return "^https://github.com/" + e.Repo + "/.github/workflows/release.yml@refs/tags/"
}

// BaseDownloadURL returns the prefix under which release assets live for
// the given tag — callers append the asset filename.
func (e *Entry) BaseDownloadURL(version string) string {
	return fmt.Sprintf("https://github.com/%s/releases/download/%s", e.Repo, version)
}
