//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const fakeVersion = "v9.9.9"

// installSandbox runs install.sh offline: a stand-in curl hands out a locally
// built release archive, and PATH holds only the sandbox and the system tools.
type installSandbox struct {
	t       *testing.T
	root    string
	fakeBin string
	asset   string
}

func newInstallSandbox(t *testing.T) *installSandbox {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &installSandbox{t: t, root: root, fakeBin: filepath.Join(root, "fakebin")}

	// The archive the installer expects: <name>/viti inside a tar.gz.
	name := fmt.Sprintf("viti-%s-%s-%s", fakeVersion, runtime.GOOS, runtime.GOARCH)
	stage := filepath.Join(root, "stage")
	s.writeScript(filepath.Join(stage, name, "viti"), "#!/bin/sh\necho 'viti version "+fakeVersion+"'\n")
	s.asset = filepath.Join(root, name+".tar.gz")
	if out, err := exec.Command("tar", "-czf", s.asset, "-C", stage, name).CombinedOutput(); err != nil {
		t.Fatalf("building the fake release archive: %v\n%s", err, out)
	}

	// With --version and both verifications skipped, the archive download is
	// the only request the installer makes.
	s.writeScript(filepath.Join(s.fakeBin, "curl"), `#!/bin/sh
out=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$out" ] || exit 22
cp "$FAKE_ASSET" "$out"
`)
	return s
}

func (s *installSandbox) writeScript(path, body string) {
	s.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		s.t.Fatal(err)
	}
	// #nosec G306 -- a test stand-in that has to be executable.
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		s.t.Fatal(err)
	}
}

// oldViti puts a stand-in for a previously installed viti in dir.
func (s *installSandbox) oldViti(dir string) string {
	s.t.Helper()
	path := filepath.Join(dir, "viti")
	s.writeScript(path, "#!/bin/sh\necho 'viti version v0.0.8'\n")
	return path
}

// run executes install.sh with pathDirs ahead of the system directories.
func (s *installSandbox) run(pathDirs []string, args ...string) string {
	s.t.Helper()
	script, err := filepath.Abs("install.sh")
	if err != nil {
		s.t.Fatal(err)
	}
	args = append([]string{script, "--version", fakeVersion, "--skip-checksum", "--skip-cosign"}, args...)
	c := exec.Command("bash", args...)
	path := append([]string{s.fakeBin}, pathDirs...)
	path = append(path, "/usr/bin", "/bin")
	c.Env = []string{
		"PATH=" + strings.Join(path, string(os.PathListSeparator)),
		"HOME=" + filepath.Join(s.root, "home"),
		"FAKE_ASSET=" + s.asset,
	}
	out, err := c.CombinedOutput()
	if err != nil {
		s.t.Fatalf("install.sh failed: %v\n%s", err, out)
	}
	return string(out)
}

func versionOf(t *testing.T, binary string) string {
	t.Helper()
	out, err := exec.Command(binary, "--version").Output()
	if err != nil {
		t.Fatalf("running %s: %v", binary, err)
	}
	return strings.TrimSpace(string(out))
}

// An installer run with no --prefix is nearly always an upgrade. Writing the
// new binary to the default directory while an older copy sits earlier on PATH
// leaves the old one running, and it keeps announcing the same upgrade.
func TestInstallScriptUpgradesTheVitiOnPathInPlace(t *testing.T) {
	// Without the in-place default the installer falls back to /usr/local/bin
	// when that is writable; never let a test write a stand-in binary there.
	if exec.Command("test", "-w", "/usr/local/bin").Run() == nil {
		t.Skip("/usr/local/bin is writable; not risking a write to it")
	}
	s := newInstallSandbox(t)
	oldDir := filepath.Join(s.root, "home", ".bin")
	defaultDir := filepath.Join(s.root, "home", ".local", "bin")
	old := s.oldViti(oldDir)

	s.run([]string{oldDir, defaultDir})

	if got := versionOf(t, old); !strings.Contains(got, fakeVersion) {
		t.Errorf("the viti first on PATH still reports %q, want it upgraded to %s", got, fakeVersion)
	}
	if _, err := os.Stat(filepath.Join(defaultDir, "viti")); err == nil {
		t.Errorf("a second copy was installed in %s", defaultDir)
	}
}

// An explicit --prefix is honoured, but installing behind another copy on
// PATH must say so: otherwise the upgrade looks like it did nothing.
func TestInstallScriptWarnsWhenAnotherVitiShadowsTheInstall(t *testing.T) {
	s := newInstallSandbox(t)
	oldDir := filepath.Join(s.root, "home", ".bin")
	newDir := filepath.Join(s.root, "home", ".local", "bin")
	old := s.oldViti(oldDir)

	out := s.run([]string{oldDir, newDir}, "--prefix", newDir)

	if got := versionOf(t, filepath.Join(newDir, "viti")); !strings.Contains(got, fakeVersion) {
		t.Fatalf("--prefix was not honoured: %s/viti reports %q", newDir, got)
	}
	if !strings.Contains(out, "warning:") || !strings.Contains(out, old) {
		t.Errorf("no warning naming the shadowing copy %s in:\n%s", old, out)
	}
}

func TestInstallScriptIsQuietWhenTheInstallIsFirstOnPath(t *testing.T) {
	s := newInstallSandbox(t)
	dir := filepath.Join(s.root, "home", ".local", "bin")

	out := s.run([]string{dir}, "--prefix", dir)

	for line := range strings.SplitSeq(out, "\n") {
		// The two verification skips the sandbox asks for are expected.
		if strings.HasPrefix(line, "warning:") && !strings.Contains(line, "skipping") {
			t.Errorf("unexpected warning for a clean install: %s", line)
		}
	}
}
