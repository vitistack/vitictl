package login

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"
)

// kubeconfigYAML builds a single-context kubeconfig whose cluster, user and
// context all share `name`, pointing at `server`.
func kubeconfigYAML(t *testing.T, name, server string) []byte {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters[name] = &clientcmdapi.Cluster{Server: server}
	cfg.AuthInfos[name] = &clientcmdapi.AuthInfo{Token: "tok-" + server}
	cfg.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: name}
	cfg.CurrentContext = name
	data, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadKubeconfig(t *testing.T, path string) *clientcmdapi.Config {
	t.Helper()
	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func hasChange(rep *MergeReport, kind, name string, replaced bool) bool {
	for _, c := range rep.Changes {
		if c.Kind == kind && c.Name == name && c.Replaced == replaced {
			return true
		}
	}
	return false
}

// The reason the command exists: a re-fetched kubeconfig must heal an expired
// entry of the same name, not be ignored the way kubectl's merge ignores it.
func TestMergeKubeconfigVerbatimReplacesSameNamedEntries(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config")
	writeFile(t, target, kubeconfigYAML(t, "admin@mgmt", "https://old:6443"))

	rep, err := MergeKubeconfigVerbatim(kubeconfigYAML(t, "admin@mgmt", "https://new:6443"), target)
	if err != nil {
		t.Fatal(err)
	}

	got := loadKubeconfig(t, target)
	if s := got.Clusters["admin@mgmt"].Server; s != "https://new:6443" {
		t.Errorf("cluster server = %q, want the incoming one", s)
	}
	if tok := got.AuthInfos["admin@mgmt"].Token; tok != "tok-https://new:6443" {
		t.Errorf("user token = %q, want the incoming one", tok)
	}
	for _, kind := range []string{"cluster", "user", "context"} {
		if !hasChange(rep, kind, "admin@mgmt", true) {
			t.Errorf("report should list %s admin@mgmt as replaced, got %+v", kind, rep.Changes)
		}
	}
}

func TestMergeKubeconfigVerbatimAddsNewAndKeepsUnrelatedEntries(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config")
	writeFile(t, target, kubeconfigYAML(t, "existing", "https://existing:6443"))

	rep, err := MergeKubeconfigVerbatim(kubeconfigYAML(t, "incoming", "https://incoming:6443"), target)
	if err != nil {
		t.Fatal(err)
	}

	got := loadKubeconfig(t, target)
	if got.Contexts["existing"] == nil || got.Contexts["incoming"] == nil {
		t.Fatalf("want both contexts, got %v", got.Contexts)
	}
	if !hasChange(rep, "context", "incoming", false) {
		t.Errorf("report should list context incoming as added, got %+v", rep.Changes)
	}
	if hasChange(rep, "context", "existing", true) || hasChange(rep, "context", "existing", false) {
		t.Errorf("report must not mention untouched entries, got %+v", rep.Changes)
	}
}

// Merging must never switch the cluster the user is pointed at.
func TestMergeKubeconfigVerbatimKeepsCurrentContext(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config")
	writeFile(t, target, kubeconfigYAML(t, "mine", "https://mine:6443"))

	rep, err := MergeKubeconfigVerbatim(kubeconfigYAML(t, "theirs", "https://theirs:6443"), target)
	if err != nil {
		t.Fatal(err)
	}
	if cur := loadKubeconfig(t, target).CurrentContext; cur != "mine" {
		t.Errorf("current-context = %q, want mine", cur)
	}
	if rep.CurrentContext != "mine" {
		t.Errorf("report current-context = %q, want mine", rep.CurrentContext)
	}
}

func TestMergeKubeconfigVerbatimAdoptsIncomingCurrentContextWhenNoneSet(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config")
	empty := kubeconfigYAML(t, "mine", "https://mine:6443")
	cfg, _ := clientcmd.Load(empty)
	cfg.CurrentContext = ""
	empty, _ = clientcmd.Write(*cfg)
	writeFile(t, target, empty)

	if _, err := MergeKubeconfigVerbatim(kubeconfigYAML(t, "theirs", "https://theirs:6443"), target); err != nil {
		t.Fatal(err)
	}
	if cur := loadKubeconfig(t, target).CurrentContext; cur != "theirs" {
		t.Errorf("current-context = %q, want theirs", cur)
	}
}

func TestMergeKubeconfigVerbatimCreatesMissingTargetWithoutBackup(t *testing.T) {
	target := filepath.Join(t.TempDir(), "sub", "config")

	rep, err := MergeKubeconfigVerbatim(kubeconfigYAML(t, "first", "https://first:6443"), target)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Backup != "" {
		t.Errorf("backup = %q, want none for a new file", rep.Backup)
	}
	if loadKubeconfig(t, target).Contexts["first"] == nil {
		t.Error("new target should hold the incoming context")
	}
	assertPrivate(t, target)
}

func TestMergeKubeconfigVerbatimBacksUpPreviousConfig(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config")
	before := kubeconfigYAML(t, "admin@mgmt", "https://old:6443")
	writeFile(t, target, before)

	rep, err := MergeKubeconfigVerbatim(kubeconfigYAML(t, "admin@mgmt", "https://new:6443"), target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(rep.Backup), "config.bak-") {
		t.Fatalf("backup = %q, want config.bak-<timestamp>", rep.Backup)
	}
	got, err := os.ReadFile(rep.Backup)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(before) {
		t.Error("backup should hold the config exactly as it was")
	}
	assertPrivate(t, rep.Backup)
	assertPrivate(t, target)
}

// Two merges in the same second must not overwrite the first one's backup:
// that backup is the only way back to the config before either merge.
func TestMergeKubeconfigVerbatimNeverOverwritesABackup(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config")
	writeFile(t, target, kubeconfigYAML(t, "a", "https://v1:6443"))

	rep1, err := MergeKubeconfigVerbatim(kubeconfigYAML(t, "a", "https://v2:6443"), target)
	if err != nil {
		t.Fatal(err)
	}
	rep2, err := MergeKubeconfigVerbatim(kubeconfigYAML(t, "a", "https://v3:6443"), target)
	if err != nil {
		t.Fatal(err)
	}
	if rep1.Backup == rep2.Backup {
		t.Fatalf("both merges wrote backup %q", rep1.Backup)
	}
	first, _ := os.ReadFile(rep1.Backup)
	if !strings.Contains(string(first), "https://v1:6443") {
		t.Error("first backup should still hold the original config")
	}
}

// A talosconfig (or any YAML with no kubeconfig entries) must be refused
// instead of being "merged" as nothing.
func TestMergeKubeconfigVerbatimRejectsInputWithoutEntries(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config")
	writeFile(t, target, kubeconfigYAML(t, "mine", "https://mine:6443"))

	_, err := MergeKubeconfigVerbatim([]byte("context: x\ncontexts:\n  x:\n    endpoints: [10.0.0.1]\n"), target)
	if err == nil || !strings.Contains(err.Error(), "talosconfig") {
		t.Fatalf("want an error suggesting this is a talosconfig, got %v", err)
	}
	if _, err := MergeKubeconfigVerbatim([]byte("foo: bar\n"), target); err == nil {
		t.Fatal("want an error for YAML with no kubeconfig entries")
	}
}

// File references in the incoming kubeconfig point at the sender's disk; they
// are embedded so the merged entry works on its own.
func TestMergeKubeconfigVerbatimEmbedsReferencedFiles(t *testing.T) {
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.crt")
	writeFile(t, ca, []byte("CA-BYTES"))
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["c"] = &clientcmdapi.Cluster{Server: "https://c:6443", CertificateAuthority: ca}
	cfg.AuthInfos["c"] = &clientcmdapi.AuthInfo{Token: "t"}
	cfg.Contexts["c"] = &clientcmdapi.Context{Cluster: "c", AuthInfo: "c"}
	data, _ := clientcmd.Write(*cfg)
	target := filepath.Join(dir, "config")

	if _, err := MergeKubeconfigVerbatim(data, target); err != nil {
		t.Fatal(err)
	}
	got := loadKubeconfig(t, target).Clusters["c"]
	if string(got.CertificateAuthorityData) != "CA-BYTES" || got.CertificateAuthority != "" {
		t.Errorf("CA should be embedded, got path %q data %q", got.CertificateAuthority, got.CertificateAuthorityData)
	}
}

// ~/.kube/config is sometimes a symlink into a dotfiles repo; replacing the
// link with a plain file would silently fork the config.
func TestMergeKubeconfigVerbatimWritesThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "real-config")
	writeFile(t, real, kubeconfigYAML(t, "mine", "https://mine:6443"))
	link := filepath.Join(dir, "config")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	if _, err := MergeKubeconfigVerbatim(kubeconfigYAML(t, "theirs", "https://theirs:6443"), link); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("target should still be a symlink (err %v)", err)
	}
	if loadKubeconfig(t, real).Contexts["theirs"] == nil {
		t.Error("the link's destination should hold the merged context")
	}
}

func assertPrivate(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("%s mode = %o, want 600", path, perm)
	}
}

const talosTarget = `context: mine
contexts:
  mine:
    endpoints: [10.0.0.1]
    ca: OLDCA
  shared:
    endpoints: [10.0.0.2]
    ca: STALE
someTopLevelKey: keep-me
`

const talosIncoming = `context: shared
contexts:
  shared:
    endpoints: [10.0.0.9]
    nodes: [10.0.0.10]
    ca: FRESH
    crt: CRT
    key: KEY
    cluster: bgo
  added:
    endpoints: [10.0.0.20]
`

func loadTalos(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMergeTalosconfigVerbatimReplacesAddsAndPreserves(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config")
	writeFile(t, target, []byte(talosTarget))

	rep, err := MergeTalosconfigVerbatim([]byte(talosIncoming), target)
	if err != nil {
		t.Fatal(err)
	}

	got := loadTalos(t, target)
	ctxs := got["contexts"].(map[string]any)
	shared := ctxs["shared"].(map[string]any)
	if shared["ca"] != "FRESH" {
		t.Errorf("shared ca = %v, want the incoming one", shared["ca"])
	}
	if shared["cluster"] != "bgo" {
		t.Errorf("fields viti does not model must survive, got %v", shared)
	}
	if ctxs["mine"] == nil || ctxs["added"] == nil {
		t.Errorf("want mine kept and added merged, got %v", ctxs)
	}
	if got["someTopLevelKey"] != "keep-me" {
		t.Errorf("unknown top-level keys must survive, got %v", got)
	}
	if got["context"] != "mine" {
		t.Errorf("context = %v, want mine (unchanged)", got["context"])
	}
	if !hasChange(rep, "context", "shared", true) || !hasChange(rep, "context", "added", false) {
		t.Errorf("report should list shared replaced and added added, got %+v", rep.Changes)
	}
	if rep.Backup == "" {
		t.Error("an existing talosconfig should be backed up")
	}
	assertPrivate(t, target)
}

func TestMergeTalosconfigVerbatimAdoptsIncomingContextWhenNoneSet(t *testing.T) {
	target := filepath.Join(t.TempDir(), "talos", "config")

	rep, err := MergeTalosconfigVerbatim([]byte(talosIncoming), target)
	if err != nil {
		t.Fatal(err)
	}
	if got := loadTalos(t, target)["context"]; got != "shared" {
		t.Errorf("context = %v, want shared", got)
	}
	if rep.CurrentContext != "shared" || rep.Backup != "" {
		t.Errorf("report = %+v, want current shared and no backup", rep)
	}
}

// A kubeconfig handed to the talos merge has a contexts list, not a map.
func TestMergeTalosconfigVerbatimRejectsAKubeconfig(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config")
	_, err := MergeTalosconfigVerbatim(kubeconfigYAML(t, "k", "https://k:6443"), target)
	if err == nil || !strings.Contains(err.Error(), "kubeconfig") {
		t.Fatalf("want an error suggesting this is a kubeconfig, got %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("a refused merge must not create the target")
	}
}
