package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/vitistack/vitictl/internal/pluginmgr"
	"github.com/vitistack/vitictl/internal/release"
)

var (
	upgradeRun       bool
	upgradeAssume    bool
	upgradeNoPlugins bool
)

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "⬆️  Check for newer releases of viti and its plugins, and upgrade",
	Long: `Checks GitHub for the latest released version of viti — and of every
installed plugin — then, if anything newer is available, prints how to
upgrade.

Pass --run to do it: viti itself is upgraded through the official installer
script (which verifies SHA-256 checksums and, when cosign is installed, the
Sigstore signature), and the plugins through the same verified path as
"viti plugin upgrade". One confirmation covers the whole operation.
--no-plugins restricts everything to viti itself.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		out := cmd.OutOrStdout()
		latest, err := release.FetchLatest(cmd.Context(), release.Repo)
		if err != nil {
			return fmt.Errorf("could not check for updates: %w", err)
		}
		local := rootCmd.Version
		_, _ = fmt.Fprintf(out, "installed: %s\n", local)
		_, _ = fmt.Fprintf(out, "latest:    %s\n", latest.Tag)

		status := release.Compare(local, latest.Tag)
		switch status {
		case release.StatusUpToDate:
			_, _ = fmt.Fprintln(out, "✅ already on the latest release")
		case release.StatusAhead:
			_, _ = fmt.Fprintln(out, "🧪 local build is ahead of the latest release")
		case release.StatusDevelopment:
			_, _ = fmt.Fprintln(out, "🛠  development build — use the installer to switch to the latest release:")
		case release.StatusOutdated:
			_, _ = fmt.Fprintln(out, "🆕 a newer release is available")
		}
		cmdline := release.UpgradeHint()
		if status == release.StatusOutdated || status == release.StatusDevelopment {
			_, _ = fmt.Fprintf(out, "   release notes: %s\n", latest.URL)
			_, _ = fmt.Fprintf(out, "   upgrade with:  %s\n", cmdline)
		}

		// The plugins recorded in ~/.vitistack/plugins ride along, so one
		// command keeps the whole toolchain current. An unreadable state dir
		// is warned about rather than blocking the self-upgrade.
		var states []*pluginmgr.State
		if !upgradeNoPlugins {
			states, err = pluginmgr.ListStates()
			if err != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "⚠️  could not read installed plugins: %v\n", err)
				states = nil
			}
		}

		if !upgradeRun {
			outdatedPlugins := 0
			for _, s := range states {
				latestV, verr := pluginmgr.LatestVersion(cmd.Context(), s.Repo)
				if verr == nil && release.Compare(s.Version, latestV) == release.StatusOutdated {
					outdatedPlugins++
				}
				_, _ = fmt.Fprintln(out, pluginStatusLine(s.Name, s.Version, latestV, verr))
			}
			if outdatedPlugins > 0 {
				_, _ = fmt.Fprintln(out, "   upgrade everything with: viti upgrade --run")
			}
			return nil
		}

		plan := planRun(status, runtime.GOOS, upgradeNoPlugins, len(states))
		if !plan.installer && !plan.plugins {
			if plan.windowsHint {
				return fmt.Errorf("--run is not supported on Windows; copy the command above into PowerShell")
			}
			_, _ = fmt.Fprintln(out, "nothing to do")
			return nil
		}

		if !upgradeAssume {
			ok, err := confirm(cmd, bundledPrompt(latest.Tag, plan.installer, len(states)))
			if err != nil {
				return err
			}
			if !ok {
				_, _ = fmt.Fprintln(out, "aborted")
				return nil
			}
		}

		if plan.windowsHint {
			_, _ = fmt.Fprintln(out,
				"⚠️  the installer cannot replace a running .exe — upgrade viti itself by copying the command above into PowerShell")
		}
		if plan.installer {
			if err := runInstaller(cmd, cmdline); err != nil {
				return fmt.Errorf("installer failed: %w — plugins were not touched; re-run to retry", err)
			}
		}
		if plan.plugins {
			upgraded, current, failed, err := upgradePlugins(cmd, states)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "plugins — upgraded: %d, up-to-date: %d, failed: %d\n",
				upgraded, current, failed)
			if failed > 0 {
				return fmt.Errorf("%d plugin upgrade(s) failed", failed)
			}
		}
		return nil
	},
}

// runPlan is what `viti upgrade --run` will execute. The self and plugin
// halves are independent: an up-to-date viti still upgrades outdated plugins.
type runPlan struct {
	// installer runs the curl|bash installer for viti itself.
	installer bool
	// windowsHint means viti itself needs upgrading but the installer cannot
	// replace a running .exe — say so instead of refusing the plugins too.
	windowsHint bool
	// plugins runs the all-plugins upgrade pass.
	plugins bool
}

func planRun(status release.Status, goos string, noPlugins bool, pluginCount int) runPlan {
	needSelf := status == release.StatusOutdated || status == release.StatusDevelopment
	return runPlan{
		installer:   needSelf && goos != "windows",
		windowsHint: needSelf && goos == "windows",
		plugins:     !noPlugins && pluginCount > 0,
	}
}

// pluginStatusLine renders one installed plugin's row in check mode. A check
// that fails (private repo without a token, offline) is reported, not fatal —
// being unable to ask is not a failure of "what is installed".
func pluginStatusLine(name, installed, latest string, err error) string {
	if err != nil {
		return fmt.Sprintf("⚠️  %s: %s (could not check the latest release: %v)", name, installed, err)
	}
	switch release.Compare(installed, latest) {
	case release.StatusOutdated:
		return fmt.Sprintf("🆕 %s: %s → %s available", name, installed, latest)
	case release.StatusAhead, release.StatusDevelopment:
		return fmt.Sprintf("🧪 %s: %s is ahead of the latest release (%s)", name, installed, latest)
	default:
		return fmt.Sprintf("✅ %s: %s (up to date)", name, installed)
	}
}

// bundledPrompt words the single confirmation to cover exactly what will run.
func bundledPrompt(latestTag string, self bool, pluginCount int) string {
	switch {
	case self && pluginCount > 0:
		return fmt.Sprintf("Upgrade viti to %s and upgrade %d installed plugin(s)?", latestTag, pluginCount)
	case self:
		return fmt.Sprintf("Run installer to upgrade to %s?", latestTag)
	default:
		return fmt.Sprintf("Upgrade %d installed plugin(s)?", pluginCount)
	}
}

// confirm prompts the user for a yes/no answer on the command's stdin.
// Non-interactive stdin is refused outright rather than read, so a piped or
// redirected invocation can never proceed with an upgrade unprompted.
func confirm(cmd *cobra.Command, prompt string) (bool, error) {
	// Ask the terminal directly rather than inferring from the file mode:
	// /dev/null is a character device but is nobody's terminal, so a
	// mode-based check waves `upgrade --run < /dev/null` through and then
	// fails on the read below with a bare "EOF".
	if in, ok := cmd.InOrStdin().(*os.File); ok && !term.IsTerminal(int(in.Fd())) {
		return false, fmt.Errorf("stdin is not a terminal; re-run with --yes to confirm non-interactively")
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N]: ", prompt)

	// A final answer without a trailing newline still counts.
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && line == "" {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// runInstaller shells out to `bash -c <cmdline>` so the pipe in the
// installer one-liner (`curl ... | bash`) is handled by the shell.
func runInstaller(cmd *cobra.Command, cmdline string) error {
	shell := "bash"
	// #nosec G204 -- cmdline is constructed from a constant repo and runtime.GOOS.
	c := exec.Command(shell, "-c", cmdline)
	c.Stdout = cmd.OutOrStdout()
	c.Stderr = cmd.ErrOrStderr()
	c.Stdin = cmd.InOrStdin()
	return c.Run()
}

func init() {
	upgradeCmd.Flags().BoolVar(&upgradeRun, "run", false,
		"execute the upgrades after printing what would happen")
	upgradeCmd.Flags().BoolVarP(&upgradeAssume, "yes", "y", false,
		"skip the confirmation prompt when used with --run")
	upgradeCmd.Flags().BoolVar(&upgradeNoPlugins, "no-plugins", false,
		"only viti itself — leave the installed plugins alone")
	rootCmd.AddCommand(upgradeCmd)
}
