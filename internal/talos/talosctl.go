package talos

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// BinaryName is the expected name of the talos CLI on PATH.
const BinaryName = "talosctl"

// HasTalosctl reports whether talosctl is discoverable on PATH.
func HasTalosctl() bool {
	_, err := exec.LookPath(BinaryName)
	return err == nil
}

// RunDashboard execs `talosctl dashboard` with the given endpoints and
// nodes, attached to the caller's stdio so the TUI renders in the
// current terminal. It blocks until the dashboard exits.
//
// talosconfigPath is passed via --talosconfig so this works without
// requiring the user to have previously merged the context into their
// ~/.talos/config (use WriteTempTalosconfig for that).
//
// endpoints populates --endpoints (Talos API addresses the client will
// connect to). nodes populates --nodes (targets the Talos API calls are
// directed at). Both are comma-separated lists in the talosctl CLI.
func RunDashboard(talosconfigPath string, endpoints, nodes []string) error {
	if talosconfigPath == "" {
		return fmt.Errorf("talosconfigPath is required")
	}
	if len(nodes) == 0 {
		return fmt.Errorf("at least one --node is required to open a Talos dashboard")
	}
	args := []string{
		"dashboard",
		"--talosconfig", talosconfigPath,
		"--nodes", strings.Join(nodes, ","),
	}
	if len(endpoints) > 0 {
		args = append(args, "--endpoints", strings.Join(endpoints, ","))
	}
	return runTalosctl(args...)
}

// runTalosctl is the shared entry point for any talosctl subcommand this
// package exposes. Kept centralised so future additions (reboot, etcd
// backup, etc.) pick up the same stdio wiring and error handling.
func runTalosctl(args ...string) error {
	if !HasTalosctl() {
		return fmt.Errorf("talosctl not found on PATH — install it from https://www.talos.dev/latest/talos-guides/install/talosctl/")
	}
	// #nosec G204 -- talosctl is the fixed binary name and args come
	// from typed fields (talosconfig path + resolved IPs/hostnames),
	// not user-shell input.
	cmd := exec.Command(BinaryName, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
