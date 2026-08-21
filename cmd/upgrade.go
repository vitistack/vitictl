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
	upgradeRunDeprecated bool
	upgradeAssume        bool
	upgradeNoPlugins     bool
	upgradeCheck         bool
)

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "⬆️  Upgrade viti and every installed plugin",
	Long: `Checks GitHub for the latest released version of viti — and of every
installed plugin — and upgrades whatever is outdated, behind a single
confirmation. viti itself goes through the official installer script (which
verifies SHA-256 checksums and, when cosign is installed, the Sigstore
signature); the plugins go through the same verified path as
"viti plugin upgrade".

--check only reports what would be upgraded and changes nothing.
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
			_, _ = fmt.Fprintln(out, "🛠  development build — the installer switches to the latest release")
		case release.StatusOutdated:
			_, _ = fmt.Fprintln(out, "🆕 a newer release is available")
		}
		cmdline := release.UpgradeHint()
		needSelf := status == release.StatusOutdated || status == release.StatusDevelopment
		if needSelf {
			_, _ = fmt.Fprintf(out, "   release notes: %s\n", latest.URL)
			_, _ = fmt.Fprintf(out, "   installer:     %s\n", cmdline)
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

		// One status line per plugin; collect the ones with work to do. A
		// plugin whose check failed (private repo without a token, offline)
		// stays in the working set — the upgrade attempt itself will say
		// exactly what is wrong.
		var needWork []*pluginmgr.State
		outdatedPlugins := 0
		for _, s := range states {
			latestV, verr := pluginmgr.LatestVersion(cmd.Context(), s.Repo)
			switch {
			case verr != nil:
				needWork = append(needWork, s)
			case release.Compare(s.Version, latestV) == release.StatusOutdated:
				outdatedPlugins++
				needWork = append(needWork, s)
			}
			_, _ = fmt.Fprintln(out, pluginStatusLine(s.Name, s.Version, latestV, verr))
		}

		if upgradeCheck {
			if showRunHint(status, outdatedPlugins) {
				_, _ = fmt.Fprintln(out, "   run 'viti upgrade' to upgrade everything")
			}
			return nil
		}

		plan := planRun(status, runtime.GOOS, upgradeNoPlugins, len(needWork))
		if !plan.installer && !plan.plugins {
			if plan.windowsHint {
				return fmt.Errorf("viti cannot replace its own running .exe — copy the installer command above into PowerShell")
			}
			_, _ = fmt.Fprintln(out, "✨ everything is up to date — nothing to do")
			return nil
		}

		if !upgradeAssume {
			ok, err := confirm(cmd, bundledPrompt(latest.Tag, plan.installer, len(needWork)))
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
				"⚠️  the installer cannot replace a running .exe — upgrade viti itself by copying the installer command above into PowerShell")
		}
		if plan.installer {
			if err := runInstaller(cmd, cmdline); err != nil {
				return fmt.Errorf("installer failed: %w — plugins were not touched; re-run to retry", err)
			}
		}
		if plan.plugins {
			upgraded, current, failed, err := upgradePlugins(cmd, needWork)
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

// runPlan is what `viti upgrade` will execute. The self and plugin halves are
// independent: an up-to-date viti still upgrades outdated plugins.
type runPlan struct {
	// installer runs the curl|bash installer for viti itself.
	installer bool
	// windowsHint means viti itself needs upgrading but the installer cannot
	// replace a running .exe — say so instead of refusing the plugins too.
	windowsHint bool
	// plugins runs the upgrade pass over the plugins that need work.
	plugins bool
}

func planRun(status release.Status, goos string, noPlugins bool, pluginsNeedingWork int) runPlan {
	needSelf := status == release.StatusOutdated || status == release.StatusDevelopment
	return runPlan{
		installer:   needSelf && goos != "windows",
		windowsHint: needSelf && goos == "windows",
		plugins:     !noPlugins && pluginsNeedingWork > 0,
	}
}

// showRunHint reports whether the --check footer should point at running
// `viti upgrade`: whenever viti itself or any plugin has an upgrade waiting.
func showRunHint(status release.Status, outdatedPlugins int) bool {
	return status == release.StatusOutdated ||
		status == release.StatusDevelopment ||
		outdatedPlugins > 0
}

// pluginStatusLine renders one installed plugin's status row. A check that
// fails (private repo without a token, offline) is reported, not fatal —
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
	// mode-based check waves `upgrade --yes < /dev/null` through and then
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
	upgradeCmd.Flags().BoolVar(&upgradeCheck, "check", false,
		"only report what would be upgraded; change nothing")
	upgradeCmd.Flags().BoolVarP(&upgradeAssume, "yes", "y", false,
		"skip the confirmation prompt")
	upgradeCmd.Flags().BoolVar(&upgradeNoPlugins, "no-plugins", false,
		"only viti itself — leave the installed plugins alone")
	// v0.0.30 briefly required --run to upgrade; upgrading is the default
	// now, so the flag survives as a hidden no-op for anything that learned
	// that syntax.
	upgradeCmd.Flags().BoolVar(&upgradeRunDeprecated, "run", false, "")
	_ = upgradeCmd.Flags().MarkDeprecated("run",
		"upgrading is now the default; use --check for a read-only report")
	rootCmd.AddCommand(upgradeCmd)
}
