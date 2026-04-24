package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl/internal/release"
)

var (
	upgradeRun    bool
	upgradeAssume bool
)

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "⬆️  Check for a newer viti release and upgrade",
	Long: `Checks GitHub for the latest released version of viti and, if a
newer release is available, prints the exact command to upgrade.

Pass --run to execute the official installer script directly from this
command. The installer verifies SHA-256 checksums and (when cosign is
installed) verifies the Sigstore signature before replacing the binary.`,
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

		switch release.Compare(local, latest.Tag) {
		case release.StatusUpToDate:
			_, _ = fmt.Fprintln(out, "✅ already on the latest release — nothing to do")
			return nil
		case release.StatusAhead:
			_, _ = fmt.Fprintln(out, "🧪 local build is ahead of the latest release — nothing to do")
			return nil
		case release.StatusDevelopment:
			_, _ = fmt.Fprintln(out, "🛠  development build — use the installer to switch to the latest release:")
		case release.StatusOutdated:
			_, _ = fmt.Fprintln(out, "🆕 a newer release is available")
		}

		cmdline := release.UpgradeHint()
		_, _ = fmt.Fprintf(out, "   release notes: %s\n", latest.URL)
		_, _ = fmt.Fprintf(out, "   upgrade with:  %s\n", cmdline)

		if !upgradeRun {
			return nil
		}
		if runtime.GOOS == "windows" {
			return fmt.Errorf("--run is not supported on Windows; copy the command above into PowerShell")
		}
		if !upgradeAssume {
			ok, err := confirm(cmd, fmt.Sprintf("Run installer to upgrade to %s?", latest.Tag))
			if err != nil {
				return err
			}
			if !ok {
				_, _ = fmt.Fprintln(out, "aborted")
				return nil
			}
		}
		return runInstaller(cmd, cmdline)
	},
}

// confirm prompts the user for a yes/no answer on the command's stdin.
// Non-interactive stdin is treated as "no" so piped invocations do not
// silently proceed with an upgrade.
func confirm(cmd *cobra.Command, prompt string) (bool, error) {
	in, ok := cmd.InOrStdin().(*os.File)
	if ok {
		if fi, err := in.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) == 0 {
			return false, fmt.Errorf("stdin is not a terminal; re-run with --yes to confirm non-interactively")
		}
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N]: ", prompt)
	reader := bufio.NewReader(cmd.InOrStdin())
	line, err := reader.ReadString('\n')
	if err != nil {
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
		"execute the installer (curl | bash) after printing instructions")
	upgradeCmd.Flags().BoolVarP(&upgradeAssume, "yes", "y", false,
		"skip the confirmation prompt when used with --run")
	rootCmd.AddCommand(upgradeCmd)
}
