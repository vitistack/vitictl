package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vitistack/vitictl/internal/login"
)

const handedOverKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: admin@mgmt
  cluster:
    server: https://new:6443
users:
- name: admin@mgmt
  user:
    token: fresh
contexts:
- name: admin@mgmt
  context:
    cluster: admin@mgmt
    user: admin@mgmt
current-context: admin@mgmt
`

// Pasting from chat is the hand-off this command was made for:
// `pbpaste | viti kubeconfig merge -`.
func TestRunConfigMergeReadsStdinAndReports(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config")
	cmd, out := stubCmd(handedOverKubeconfig)

	if err := runConfigMerge(cmd, "-", target, login.MergeKubeconfigVerbatim); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"added context admin@mgmt", "merged into " + target} {
		if !strings.Contains(got, want) {
			t.Errorf("output should contain %q, got:\n%s", want, got)
		}
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("target should have been written: %v", err)
	}
}

func TestRunConfigMergeReadsFileAndReportsReplacementAndBackup(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config")
	src := filepath.Join(dir, "handed-over.yaml")
	for _, p := range []string{target, src} {
		if err := os.WriteFile(p, []byte(handedOverKubeconfig), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd, out := stubCmd("")

	if err := runConfigMerge(cmd, src, target, login.MergeKubeconfigVerbatim); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"replaced context admin@mgmt", "3 replaced, 0 added", "backup: " + target + ".bak-"} {
		if !strings.Contains(got, want) {
			t.Errorf("output should contain %q, got:\n%s", want, got)
		}
	}
}

func TestRunConfigMergeRefusesEmptyInput(t *testing.T) {
	cmd, _ := stubCmd("")
	err := runConfigMerge(cmd, "-", filepath.Join(t.TempDir(), "config"), login.MergeKubeconfigVerbatim)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("want an empty-input error, got %v", err)
	}
}

func TestConfigMergeCommandsAreRegistered(t *testing.T) {
	for _, path := range [][]string{{"kubeconfig", "merge"}, {"talosconfig", "merge"}} {
		c, _, err := rootCmd.Find(path)
		if err != nil || c.Name() != "merge" {
			t.Errorf("viti %s should exist (err %v)", strings.Join(path, " "), err)
			continue
		}
		if c.Flags().Lookup(path[0]) == nil {
			t.Errorf("viti %s should take --%s to pick the target file", strings.Join(path, " "), path[0])
		}
	}
}
