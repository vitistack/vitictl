// Package selfupgrade provides the version and upgrade commands every viti
// plugin ships: `viti <name> version [--check]` and
// `viti <name> upgrade [--run] [--yes]`, delegating actual binary
// replacement to `viti plugin upgrade <name>`.
package selfupgrade

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/vitistack/vitictl/pkg/plugin/release"
)

// Options parameterizes the version and upgrade commands for one plugin.
type Options struct {
	Name    string // plugin name in viti's index, e.g. "acme"
	Repo    string // GitHub owner/name, e.g. "vitistack/vitictl-acme"
	Version string // ldflags-injected build version, e.g. "v0.1.1"
}

// NewVersionCmd builds the `version` command for the plugin described by o.
func NewVersionCmd(o Options) *cobra.Command {
	var check bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the viti-" + o.Name + " version",
		Long: fmt.Sprintf(`Print the installed viti-%s version.

With --check, also ask GitHub for the latest published release and report
whether this build is current. If the plugin's repository is private, the
check needs a GitHub token: set GH_TOKEN (or GITHUB_TOKEN), or run
"gh auth login". Without one the check says so and exits zero — being offline
or unauthenticated is not a failure of "version".`, o.Name),
		Example: fmt.Sprintf(`  viti %s version
  viti %s version --check`, o.Name, o.Name),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			// Same wording as the root command's version template, so
			// "viti <name> version" and "viti <name> --version" never disagree.
			if _, err := fmt.Fprintf(out, "viti-%s version %s\n", o.Name, o.Version); err != nil {
				return err
			}
			if !check {
				return nil
			}
			return printReleaseCheck(cmd.Context(), out, o)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false,
		"check GitHub for a newer release and print upgrade instructions")
	return cmd
}

// printReleaseCheck reports whether the local build is up to date.
//
// A network or authentication failure is printed but not returned:
// `version --check` must never exit non-zero merely because the user is
// offline or has no GitHub token. Anything that stops the check is reported
// on the same stream as the version itself, so it is never silent.
func printReleaseCheck(ctx context.Context, out io.Writer, o Options) error {
	latest, err := release.FetchLatest(ctx, o.Repo)
	if err != nil {
		_, err := fmt.Fprintf(out, "⚠️  could not check for updates: %v\n", err)
		return err
	}
	return printReleaseStatus(out, o, latest)
}

// printReleaseStatus renders the comparison between the local build and the
// latest release. It is split from the fetch so every branch stays testable
// without reaching the network.
func printReleaseStatus(out io.Writer, o Options, latest *release.Latest) error {
	switch release.Compare(o.Version, latest.Tag) {
	case release.StatusUpToDate:
		_, _ = fmt.Fprintf(out, "✅ you are on the latest release (%s)\n", latest.Tag)
	case release.StatusOutdated:
		_, _ = fmt.Fprintf(out, "🆕 a newer release is available: %s (you have %s)\n", latest.Tag, o.Version)
		_, _ = fmt.Fprintf(out, "   release notes: %s\n", latest.URL)
		_, _ = fmt.Fprintf(out, "   upgrade with:  %s\n", release.UpgradeHint(o.Name))
		_, _ = fmt.Fprintf(out, "   or run:        viti %s upgrade --run\n", o.Name)
	case release.StatusAhead:
		_, _ = fmt.Fprintf(out, "🧪 your build (%s) is ahead of the latest release (%s)\n", o.Version, latest.Tag)
	case release.StatusDevelopment:
		_, _ = fmt.Fprintf(out, "🛠  development build (%s); latest release is %s\n", o.Version, latest.Tag)
		_, _ = fmt.Fprintf(out, "   release notes: %s\n", latest.URL)
	}
	return nil
}

// NewUpgradeCmd builds the `upgrade` command for the plugin described by o.
func NewUpgradeCmd(o Options) *cobra.Command {
	var (
		run    bool
		assume bool
	)

	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "⬆️  Check for a newer viti-" + o.Name + " release and upgrade",
		Long: fmt.Sprintf(`Checks GitHub for the latest released version of the viti-%s plugin
and, if a newer release is available, prints the command that upgrades it.

viti-%s ships no installer of its own. It is a viti plugin, so upgrades go
through %q, which downloads the release, verifies its
SHA-256 checksum and (when cosign is installed) its Sigstore signature, and
replaces the binary atomically. Pass --run to have this command invoke that
for you.

If the plugin's repository is private, the check needs a GitHub token: set
GH_TOKEN (or GITHUB_TOKEN), or run "gh auth login".`, o.Name, o.Name, release.UpgradeHint(o.Name)),
		Example: fmt.Sprintf(`  viti %s upgrade
  viti %s upgrade --run
  viti %s upgrade --run --yes`, o.Name, o.Name, o.Name),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			latest, err := release.FetchLatest(cmd.Context(), o.Repo)
			if err != nil {
				return fmt.Errorf("could not check for updates: %w", err)
			}
			_, _ = fmt.Fprintf(out, "installed: %s\n", o.Version)
			_, _ = fmt.Fprintf(out, "latest:    %s\n", latest.Tag)

			switch release.Compare(o.Version, latest.Tag) {
			case release.StatusUpToDate:
				_, _ = fmt.Fprintln(out, "✅ already on the latest release — nothing to do")
				return nil
			case release.StatusAhead:
				_, _ = fmt.Fprintln(out, "🧪 local build is ahead of the latest release — nothing to do")
				return nil
			case release.StatusDevelopment:
				_, _ = fmt.Fprintln(out, "🛠  development build — switch to the latest release with:")
			case release.StatusOutdated:
				_, _ = fmt.Fprintln(out, "🆕 a newer release is available")
			}

			hint := release.UpgradeHint(o.Name)
			_, _ = fmt.Fprintf(out, "   release notes: %s\n", latest.URL)
			_, _ = fmt.Fprintf(out, "   upgrade with:  %s\n", hint)

			if !run {
				return nil
			}
			if runtime.GOOS == "windows" {
				// The upgrade replaces this very binary. Unix keeps our
				// running image alive when the file is renamed over; Windows
				// refuses to replace a running .exe, so the upgrade has to be
				// started from a viti process rather than from inside us.
				return fmt.Errorf("--run is not supported on Windows; run %q yourself", hint)
			}
			if !assume {
				ok, err := Confirm(cmd, fmt.Sprintf("Run %q to upgrade to %s?", hint, latest.Tag))
				if err != nil {
					return err
				}
				if !ok {
					_, _ = fmt.Fprintln(out, "aborted")
					return nil
				}
			}
			return runPluginUpgrade(cmd, o)
		},
	}
	cmd.Flags().BoolVar(&run, "run", false,
		fmt.Sprintf("run `%s` after printing instructions", release.UpgradeHint(o.Name)))
	cmd.Flags().BoolVarP(&assume, "yes", "y", false,
		"skip the confirmation prompt when used with --run")
	return cmd
}

// Confirm asks for a yes/no answer on the command's stdin.
//
// Non-interactive stdin is refused rather than assumed either way, so a
// piped or CI invocation never replaces a binary without having been told
// to. --yes is the documented way through.
func Confirm(cmd *cobra.Command, prompt string) (bool, error) {
	// Ask the terminal directly rather than inferring from the file mode:
	// /dev/null is a character device but is nobody's terminal, so a
	// mode-based check waves `upgrade --run < /dev/null` through and then
	// fails on the read with a bare "EOF".
	if in, ok := cmd.InOrStdin().(*os.File); ok && !term.IsTerminal(int(in.Fd())) {
		return false, fmt.Errorf("stdin is not a terminal; re-run with --yes to confirm non-interactively")
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N]: ", prompt)

	// A final line without a trailing newline still counts as an answer.
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && line == "" {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// runPluginUpgrade hands the upgrade to viti's plugin manager, which owns
// downloading, verifying and replacing plugin binaries.
//
// Nothing goes through a shell: unlike vitictl's own `upgrade --run`, which
// has to pipe curl into bash, there is no pipe here, so exec is used
// directly and there is no quoting or injection surface at all.
func runPluginUpgrade(cmd *cobra.Command, o Options) error {
	viti, err := exec.LookPath("viti")
	if err != nil {
		return fmt.Errorf("viti was not found on PATH; run %q from a shell where viti is installed",
			release.UpgradeHint(o.Name))
	}
	// #nosec G204 -- viti is resolved from PATH and invoked with fixed arguments.
	c := exec.CommandContext(cmd.Context(), viti, "plugin", "upgrade", o.Name)
	c.Stdout = cmd.OutOrStdout()
	c.Stderr = cmd.ErrOrStderr()
	c.Stdin = cmd.InOrStdin()
	return c.Run()
}
