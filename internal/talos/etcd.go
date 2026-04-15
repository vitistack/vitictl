package talos

import "fmt"

// EtcdSnapshot runs `talosctl -n <node> etcd snapshot <outputPath>` to take
// a clean point-in-time snapshot of etcd via the etcd snapshot RPC. The
// resulting file is written locally to outputPath. Use this on a healthy
// cluster.
func EtcdSnapshot(talosconfigPath, node, outputPath string) error {
	if talosconfigPath == "" || node == "" || outputPath == "" {
		return fmt.Errorf("talosconfigPath, node, and outputPath are required")
	}
	return runTalosctl(
		"--talosconfig", talosconfigPath,
		"-n", node,
		"etcd", "snapshot", outputPath,
	)
}

// EtcdSnapshotRaw is the unhealthy-cluster fallback documented in the
// Talos disaster-recovery guide: copy the raw etcd snap file off the
// node when `etcd snapshot` cannot complete. The file lacks the
// integrity hash that talosctl normally appends, so a subsequent
// BootstrapRecover must be invoked with skipHashCheck=true.
//
// outputPath is the local destination file. talosctl cp sends the
// remote file's basename to the local path argument verbatim, so a
// fully-qualified file path is fine.
func EtcdSnapshotRaw(talosconfigPath, node, outputPath string) error {
	if talosconfigPath == "" || node == "" || outputPath == "" {
		return fmt.Errorf("talosconfigPath, node, and outputPath are required")
	}
	return runTalosctl(
		"--talosconfig", talosconfigPath,
		"-n", node,
		"cp", "/var/lib/etcd/member/snap/db", outputPath,
	)
}

// BootstrapRecover runs `talosctl -n <node> bootstrap --recover-from <snapshotPath>`
// against a single control-plane node to restore etcd from a snapshot.
//
// PRECONDITIONS (per Talos disaster-recovery docs):
//   - All etcd service instances on the cluster's control planes must be
//     in the Preparing state (i.e. etcd is not running).
//   - No control plane node may have machine type `init`; convert with
//     `talosctl edit mc --mode=staged` first if needed.
//   - For hardware-loss recovery, prepare fresh CP nodes before invoking.
//
// skipHashCheck must be true when the snapshot was produced by
// EtcdSnapshotRaw (raw etcd file, no hash); leave false for snapshots
// produced by EtcdSnapshot.
func BootstrapRecover(talosconfigPath, node, snapshotPath string, skipHashCheck bool) error {
	if talosconfigPath == "" || node == "" || snapshotPath == "" {
		return fmt.Errorf("talosconfigPath, node, and snapshotPath are required")
	}
	args := []string{
		"--talosconfig", talosconfigPath,
		"-n", node,
		"bootstrap",
		"--recover-from", snapshotPath,
	}
	if skipHashCheck {
		args = append(args, "--recover-skip-hash-check")
	}
	return runTalosctl(args...)
}
