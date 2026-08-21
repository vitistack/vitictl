// Package viticli shells out to the parent viti CLI on behalf of a plugin.
//
// Functionality a plugin needs from viti (or from another plugin) is driven
// through the real command rather than reimplemented — one implementation,
// in the repository that owns it. This package owns the exec plumbing and
// the failure classification: user cancels stay quiet, children that died
// without reporting are loud, and cobra's bare "unknown command" is turned
// into an install-or-upgrade hint by PluginDiagnosis.
package viticli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Binary is the executable to invoke. A variable so tests can point it at a
// stub instead of the real thing.
var Binary = "viti"

// ErrNotInstalled is returned when viti is not on PATH.
var ErrNotInstalled = errors.New("the viti CLI was not found on PATH")

// ErrChildFailed marks a failure the child command already reported on its
// own stderr — the wrapper's root prints nothing more, only exits non-zero.
var ErrChildFailed = errors.New("viti kubevirt already reported the failure")

// Streams are the caller's I/O, wired straight through rather than captured:
// the child command runs interactive pickers and confirmation prompts.
type Streams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// Path resolves the viti binary.
func Path() (string, error) {
	p, err := exec.LookPath(Binary)
	if err != nil {
		return "", fmt.Errorf(
			"%w — install viti and the plugin this command drives", ErrNotInstalled)
	}
	return p, nil
}

// DiagnoseFunc turns a child failure into the right report, once the failure
// is known not to be a cancel or a kill (see classify). nil means every such
// failure is the child's own report.
type DiagnoseFunc func(ctx context.Context, bin string, childErr error) error

// Run executes `viti args...` attached to the caller's streams.
func Run(ctx context.Context, s Streams, args []string, diagnose DiagnoseFunc) error {
	bin, err := Path()
	if err != nil {
		return err
	}
	// #nosec G204 -- bin is resolved from PATH; args are a fixed subcommand
	// plus flag values, never a shell string.
	c := exec.CommandContext(ctx, bin, args...)
	c.Stdin = s.In
	c.Stdout = s.Out
	c.Stderr = s.Err
	if c.Stdin == nil {
		c.Stdin = os.Stdin
	}
	if err := c.Run(); err != nil {
		return classify(ctx, bin, err, diagnose)
	}
	return nil
}

// classify turns a child failure into the right report. The child normally
// prints its own "❌ Error:" line on the shared stderr, so most failures are
// marked ErrChildFailed and repeated by nobody — but cobra's bare "unknown
// command" explains nothing, and it means two different things (plugin too
// old vs never installed) with two different fixes. diagnose, when given,
// probes --help at each level to tell the cases apart.
func classify(ctx context.Context, bin string, childErr error, diagnose DiagnoseFunc) error {
	// Cancelled by the user (Ctrl+C in the child's picker or prompt): not a
	// failure to diagnose — and any probe would be killed by the same
	// cancellation and misread as a plugin problem.
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %v", ErrChildFailed, childErr)
	}

	// A child that never exited normally (killed, or failed before exec)
	// reported nothing; staying silent would make the wrapper exit non-zero
	// with no output at all.
	var exitErr *exec.ExitError
	if !errors.As(childErr, &exitErr) || !exitErr.Exited() {
		return fmt.Errorf("viti kubevirt was terminated before it could report anything: %w", childErr)
	}

	if diagnose != nil {
		return diagnose(ctx, bin, childErr)
	}
	return fmt.Errorf("%w: %v", ErrChildFailed, childErr)
}

// PluginDiagnosis distinguishes "plugin too old" (upgrade hint), "plugin not
// installed" (install hint), and "real failure" (ErrChildFailed) by probing
// --help at each level of probe; probe[0] is the plugin name, e.g.
// []string{"kubevirt", "vm", "changemachineclass"}.
func PluginDiagnosis(probe []string) DiagnoseFunc {
	return func(ctx context.Context, bin string, childErr error) error {
		sub := strings.Join(probe[1:], " ")
		switch {
		case probeOK(ctx, bin, append(append([]string{}, probe...), "--help")...):
			// The subcommand exists, so the child's own report already said
			// what went wrong.
			return fmt.Errorf("%w: %v", ErrChildFailed, childErr)
		case probeOK(ctx, bin, probe[0], "--help"):
			return fmt.Errorf(
				"your viti %[1]s plugin does not know '%[2]s' yet — "+
					"upgrade it with 'viti plugin upgrade %[1]s' (or 'viti %[1]s upgrade'): %[3]v",
				probe[0], sub, childErr)
		case probeOK(ctx, bin, "--help"):
			return fmt.Errorf(
				"the viti %[1]s plugin is not installed — "+
					"install it with 'viti plugin install %[1]s': %[2]v", probe[0], childErr)
		default:
			// viti itself cannot even print its help; there is nothing to
			// diagnose beyond whatever it already printed.
			return fmt.Errorf("%w: %v", ErrChildFailed, childErr)
		}
	}
}

// probeOK runs the binary with the given arguments, all output discarded,
// reporting whether it succeeded.
func probeOK(ctx context.Context, bin string, args ...string) bool {
	// #nosec G204 -- bin is resolved from PATH; the args are fixed.
	probe := exec.CommandContext(ctx, bin, args...)
	probe.Stdout = io.Discard
	probe.Stderr = io.Discard
	return probe.Run() == nil
}
