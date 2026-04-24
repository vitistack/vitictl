package pluginmgr

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// InstallOptions controls how Install fetches and installs a plugin
// binary. Zero values use safe defaults: latest version resolved via
// GitHub, auto-picked prefix, sha256 enforced, cosign best-effort.
type InstallOptions struct {
	// Version to install. Empty means "resolve latest from GitHub".
	Version string
	// Prefix is the install directory. Empty means auto-detect from the
	// currently running viti binary.
	Prefix string
	// SkipChecksum disables SHA-256 verification. Not recommended.
	SkipChecksum bool
	// SkipCosign disables Sigstore signature verification. The installer
	// also silently skips cosign if the cosign binary is not on PATH.
	SkipCosign bool
	// Stdout/Stderr receive human-readable progress messages. Nil writes
	// to os.Stderr (progress) / are suppressed (stdout).
	Stdout io.Writer
	Stderr io.Writer
}

// Install downloads the release asset for entry@version, verifies it,
// extracts the binary, and writes it to Prefix. It returns the state
// describing the installed binary but does NOT persist the state — the
// caller owns that decision (e.g. so dry-runs can preview without
// touching disk state).
func Install(ctx context.Context, entry *Entry, opts InstallOptions) (*State, error) {
	if entry == nil {
		return nil, errors.New("nil entry")
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	version := strings.TrimSpace(opts.Version)
	if version == "" {
		logf(stderr, "resolving latest release for %s", entry.Repo)
		var err error
		version, err = resolveLatest(ctx, entry.Repo)
		if err != nil {
			return nil, err
		}
	}

	goos, goarch := runtime.GOOS, runtime.GOARCH
	archive := entry.ArchiveName(version, goos, goarch)
	inner := entry.InnerBinaryPath(version, goos, goarch)
	sumsName := entry.ChecksumsName(version)
	base := entry.BaseDownloadURL(version)

	prefix := opts.Prefix
	if prefix == "" {
		p, err := defaultPrefix()
		if err != nil {
			return nil, err
		}
		prefix = p
	}
	if err := os.MkdirAll(prefix, 0o750); err != nil {
		return nil, fmt.Errorf("creating install prefix %q: %w", prefix, err)
	}

	tmp, err := os.MkdirTemp("", "viti-plugin-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	logf(stderr, "installing %s %s for %s/%s", entry.Name, version, goos, goarch)
	logf(stderr, "downloading %s", archive)
	archivePath := filepath.Join(tmp, archive)
	if err := download(ctx, base+"/"+archive, archivePath); err != nil {
		return nil, fmt.Errorf("downloading archive: %w", err)
	}

	var archiveSHA string
	if !opts.SkipChecksum {
		sumsPath := filepath.Join(tmp, sumsName)
		if err := download(ctx, base+"/"+sumsName, sumsPath); err != nil {
			return nil, fmt.Errorf("downloading checksums: %w", err)
		}
		expected, err := checksumFor(sumsPath, archive)
		if err != nil {
			return nil, err
		}
		actual, err := sha256File(archivePath)
		if err != nil {
			return nil, err
		}
		if expected != actual {
			return nil, fmt.Errorf("SHA-256 mismatch for %s: expected %s, got %s", archive, expected, actual)
		}
		archiveSHA = actual
		logf(stderr, "SHA-256 ok")
	} else {
		logf(stderr, "skipping SHA-256 verification")
	}

	if !opts.SkipCosign {
		if err := verifyCosign(ctx, stderr, base, archive, archivePath, entry.CosignIdentity(), tmp); err != nil {
			return nil, err
		}
	} else {
		logf(stderr, "skipping cosign signature verification")
	}

	extractedBinary, err := extractBinary(archivePath, inner, tmp)
	if err != nil {
		return nil, err
	}

	// On Unix, the install destination is `viti-<name>` next to viti
	// itself. On Windows, append .exe.
	dstName := "viti-" + entry.Name
	if goos == "windows" {
		dstName += ".exe"
	}
	dst := filepath.Join(prefix, dstName)
	if err := installFile(extractedBinary, dst); err != nil {
		return nil, err
	}
	logf(stderr, "installed %s", dst)

	return &State{
		Name:        entry.Name,
		Repo:        entry.Repo,
		Version:     version,
		BinaryPath:  dst,
		SHA256:      archiveSHA,
		InstalledAt: time.Now().UTC(),
	}, nil
}

// Uninstall removes the installed binary for state and returns.
// Callers are responsible for removing the state file separately.
func Uninstall(state *State) error {
	if state == nil || state.BinaryPath == "" {
		return errors.New("nil state or missing binary path")
	}
	if err := os.Remove(state.BinaryPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", state.BinaryPath, err)
	}
	return nil
}

// resolveLatest pulls the latest release tag for repo directly, without
// pulling in the release package to avoid an import cycle with cmd
// (pluginmgr is imported by cmd; keeping it free of that dep makes
// future refactors easier).
func resolveLatest(ctx context.Context, repo string) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github API returned %s for %s", resp.Status, repo)
	}
	var out struct {
		TagName string `json:"tag_name"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.TagName == "" {
		return "", fmt.Errorf("no tag_name in release response for %s", repo)
	}
	return out.TagName, nil
}

// defaultPrefix installs plugin binaries next to the running viti
// binary — wherever the user already trusted the main CLI to live.
// Falls back to ~/.local/bin if os.Executable() is unusable.
func defaultPrefix() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		dir := filepath.Dir(exe)
		if dir != "" && dir != "." {
			return dir, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin"), nil
}

// download streams url to dst, returning an error for any non-200 response.
func download(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	f, err := os.Create(dst) // #nosec G304 -- dst is constructed under a freshly created tmp dir.
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return f.Close()
}

// checksumFor parses a SHA256SUMS file (format: "<hex>  <filename>" or
// "<hex> *<filename>") and returns the expected hash for asset.
func checksumFor(path, asset string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- path is constructed under a freshly created tmp dir.
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == asset {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("no SHA-256 entry for %s in %s", asset, filepath.Base(path))
}

// sha256File returns the lowercase hex SHA-256 of the file at path.
func sha256File(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- path is constructed under a freshly created tmp dir.
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifyCosign shells out to `cosign verify-blob` using the release's
// .cosign.bundle asset. Matches install.sh's behavior: if cosign is not
// on PATH, warn and skip (don't fail the install).
func verifyCosign(ctx context.Context, stderr io.Writer, base, archive, archivePath, identity, tmp string) error {
	if _, err := exec.LookPath("cosign"); err != nil {
		logf(stderr, "⚠️  cosign not found on PATH — skipping signature verification")
		return nil
	}
	bundleURL := base + "/" + archive + ".cosign.bundle"
	bundle := filepath.Join(tmp, archive+".cosign.bundle")
	if err := download(ctx, bundleURL, bundle); err != nil {
		logf(stderr, "⚠️  cosign bundle not available (%v) — skipping signature verification", err)
		return nil
	}
	logf(stderr, "verifying Sigstore signature with cosign")
	// #nosec G204 -- all arguments are constructed from trusted internal values
	// (constants, tmp paths, and the entry's identity regex, validated at index load).
	c := exec.CommandContext(ctx, "cosign", "verify-blob",
		"--bundle", bundle,
		"--certificate-identity-regexp", identity,
		"--certificate-oidc-issuer", "https://token.actions.githubusercontent.com",
		archivePath,
	)
	c.Stdout = io.Discard
	c.Stderr = stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("cosign verify-blob failed: %w", err)
	}
	logf(stderr, "cosign signature ok")
	return nil
}

// extractBinary opens a gzip+tar archive and writes the entry whose name
// matches innerPath to disk under tmp. Only the specific inner binary is
// extracted; other files (README, LICENSE) are ignored.
func extractBinary(archivePath, innerPath, tmp string) (string, error) {
	f, err := os.Open(archivePath) // #nosec G304 -- archivePath is under a freshly created tmp dir.
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("opening gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("reading tar: %w", err)
		}
		// Normalize the header name: archives written by GNU tar may use
		// "./" prefixes; we compare on the cleaned path.
		name := filepath.ToSlash(filepath.Clean(hdr.Name))
		if name != innerPath {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return "", fmt.Errorf("archive entry %s is not a regular file", name)
		}
		out := filepath.Join(tmp, filepath.Base(innerPath))
		// #nosec G302,G304 -- plugin binaries require the executable bit; out is under a freshly created tmp dir.
		w, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return "", err
		}
		// #nosec G110 -- release archive size is bounded by GitHub; decompression bomb risk is low and the binary is verified by SHA-256 above.
		if _, err := io.Copy(w, tr); err != nil {
			_ = w.Close()
			return "", err
		}
		if err := w.Close(); err != nil {
			return "", err
		}
		return out, nil
	}
	return "", fmt.Errorf("archive does not contain %s", innerPath)
}

// installFile atomically replaces dst with the contents of src.
func installFile(src, dst string) error {
	in, err := os.Open(src) // #nosec G304 -- src is under a freshly created tmp dir.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	tmpDst := dst + ".tmp"
	// #nosec G302,G304 -- plugin binaries require the executable bit; dst is under the user-chosen install prefix.
	out, err := os.OpenFile(tmpDst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmpDst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmpDst)
		return err
	}
	if err := os.Rename(tmpDst, dst); err != nil {
		_ = os.Remove(tmpDst)
		return err
	}
	return nil
}

func logf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	_, _ = fmt.Fprintf(w, "==> "+format+"\n", args...)
}
