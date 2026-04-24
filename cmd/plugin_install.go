package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl/internal/pluginmgr"
	"github.com/vitistack/vitictl/internal/release"
)

var (
	installPrefix       string
	installSkipChecksum bool
	installSkipCosign   bool
)

var pluginInstallCmd = &cobra.Command{
	Use:   "install <name>[@<version>]",
	Short: "Install a plugin from the curated index",
	Long: `Fetches the curated plugin index (plugins.yaml) from the vitictl
repo, resolves <name> to a GitHub release, downloads and verifies the
release archive (SHA-256 always, cosign signature when available), and
installs the binary next to viti itself (or into --prefix).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, version := splitNameVersion(args[0])
		idx, err := pluginmgr.FetchIndex(cmd.Context())
		if err != nil {
			return fmt.Errorf("fetching plugin index: %w", err)
		}
		entry, ok := idx.Find(name)
		if !ok {
			return fmt.Errorf("plugin %q is not in the curated index (see `viti plugin list --available`)", name)
		}
		state, err := pluginmgr.Install(cmd.Context(), entry, pluginmgr.InstallOptions{
			Version:      version,
			Prefix:       installPrefix,
			SkipChecksum: installSkipChecksum,
			SkipCosign:   installSkipCosign,
			Stderr:       cmd.ErrOrStderr(),
		})
		if err != nil {
			return err
		}
		if err := pluginmgr.WriteState(state); err != nil {
			return fmt.Errorf("saving plugin state: %w", err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"✅ %s %s installed — try `viti %s --help`\n", state.Name, state.Version, state.Name)
		return nil
	},
}

var pluginUpgradeCmd = &cobra.Command{
	Use:   "upgrade [<name>...]",
	Short: "Upgrade one or more installed plugins",
	Long: `With no arguments, upgrades every plugin recorded in ~/.vitistack/plugins.
With one or more names, upgrades only those. Plugins already on the
latest release are skipped.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		targets, err := resolveUpgradeTargets(args)
		if err != nil {
			return err
		}
		if len(targets) == 0 {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no installed plugins to upgrade")
			return nil
		}
		idx, err := pluginmgr.FetchIndex(cmd.Context())
		if err != nil {
			return fmt.Errorf("fetching plugin index: %w", err)
		}
		var upgraded, skipped, failed int
		for _, state := range targets {
			if err := upgradeOne(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), idx, state); err != nil {
				failed++
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "❌ %s: %v\n", state.Name, err)
				continue
			}
			// upgradeOne distinguishes no-op vs upgraded via its own output;
			// to decide the summary we re-read state and compare.
			post, err := pluginmgr.ReadState(state.Name)
			if err == nil && post != nil && post.Version != state.Version {
				upgraded++
			} else {
				skipped++
			}
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"done — upgraded: %d, up-to-date: %d, failed: %d\n", upgraded, skipped, failed)
		if failed > 0 {
			return errors.New("one or more upgrades failed")
		}
		return nil
	},
}

var pluginUninstallCmd = &cobra.Command{
	Use:     "uninstall <name>",
	Aliases: []string{"remove", "rm"},
	Short:   "Remove an installed plugin",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		state, err := pluginmgr.ReadState(args[0])
		if err != nil {
			return err
		}
		if state == nil {
			return fmt.Errorf("plugin %q is not installed (no state file)", args[0])
		}
		if err := pluginmgr.Uninstall(state); err != nil {
			return err
		}
		if err := pluginmgr.DeleteState(state.Name); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✅ removed %s (%s)\n", state.Name, state.BinaryPath)
		return nil
	},
}

// resolveUpgradeTargets returns the set of plugin states to upgrade. An
// empty names slice expands to all installed plugins.
func resolveUpgradeTargets(names []string) ([]*pluginmgr.State, error) {
	if len(names) == 0 {
		return pluginmgr.ListStates()
	}
	var out []*pluginmgr.State
	for _, n := range names {
		s, err := pluginmgr.ReadState(n)
		if err != nil {
			return nil, err
		}
		if s == nil {
			return nil, fmt.Errorf("plugin %q is not installed", n)
		}
		out = append(out, s)
	}
	return out, nil
}

// upgradeOne checks the plugin's repo for a newer release and, if
// available, reinstalls it. No-op when already current.
func upgradeOne(ctx context.Context, stdout, stderr io.Writer, idx *pluginmgr.Index, state *pluginmgr.State) error {
	entry, ok := idx.Find(state.Name)
	if !ok {
		// Entry vanished from the index — still try to upgrade from the
		// repo recorded in state by synthesising a minimal entry.
		entry = &pluginmgr.Entry{Name: state.Name, Repo: state.Repo}
	}
	latest, err := release.FetchLatest(ctx, entry.Repo)
	if err != nil {
		return fmt.Errorf("checking %s: %w", entry.Repo, err)
	}
	switch release.Compare(state.Version, latest.Tag) {
	case release.StatusUpToDate:
		_, _ = fmt.Fprintf(stdout, "✅ %s %s — already up to date\n", state.Name, state.Version)
		return nil
	case release.StatusAhead:
		_, _ = fmt.Fprintf(stdout, "🧪 %s %s is ahead of latest (%s) — skipping\n", state.Name, state.Version, latest.Tag)
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "⬆️  %s: %s -> %s\n", state.Name, state.Version, latest.Tag)
	newState, err := pluginmgr.Install(ctx, entry, pluginmgr.InstallOptions{
		Version: latest.Tag,
		Prefix:  dirOf(state.BinaryPath),
		Stderr:  stderr,
	})
	if err != nil {
		return err
	}
	return pluginmgr.WriteState(newState)
}

// splitNameVersion parses "name" or "name@v1.2.3" into (name, version).
func splitNameVersion(s string) (name, version string) {
	for i := 0; i < len(s); i++ {
		if s[i] == '@' {
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}

// dirOf returns the directory component of p. Used to keep plugin
// upgrades installing back to their original location.
func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return ""
}

func init() {
	pluginInstallCmd.Flags().StringVar(&installPrefix, "prefix", "",
		"install directory (default: same directory as viti)")
	pluginInstallCmd.Flags().BoolVar(&installSkipChecksum, "skip-checksum", false,
		"skip SHA-256 verification (not recommended)")
	pluginInstallCmd.Flags().BoolVar(&installSkipCosign, "skip-cosign", false,
		"skip Sigstore signature verification")

	pluginCmd.AddCommand(pluginInstallCmd)
	pluginCmd.AddCommand(pluginUpgradeCmd)
	pluginCmd.AddCommand(pluginUninstallCmd)
}
