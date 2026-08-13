package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl/internal/plugin"
	"github.com/vitistack/vitictl/internal/pluginmgr"
	"github.com/vitistack/vitictl/internal/release"
)

var (
	installPrefix       string
	installSkipChecksum bool
	installSkipCosign   bool
	installNoAliases    bool
)

// aliasConflicts reports which of an entry's aliases are already claimed, by
// viti's own commands or by another plugin already on PATH.
//
// The plugin's own name is excluded from the "other plugins" set so that
// reinstalling or upgrading never conflicts with the copy being replaced.
func aliasConflicts(entry *pluginmgr.Entry) []pluginmgr.Conflict {
	// The binary this plugin already occupies, if any. Anything resolving to
	// it is this plugin under another name, not a competitor.
	var ownBinary string
	var ownAliases []string
	if state, err := pluginmgr.ReadState(entry.Name); err == nil && state != nil {
		ownBinary = state.BinaryPath
		ownAliases = state.Aliases
	}

	others := map[string]string{}
	if found, err := plugin.List(); err == nil {
		for _, p := range found {
			if p.Name == entry.Name {
				continue
			}
			if _, dup := others[p.Name]; dup {
				continue // first on PATH wins, matching dispatch
			}
			others[p.Name] = p.Path
		}
	}

	// An alias this plugin already answers to is the link about to be
	// rewritten, not a conflict. That covers both the links viti recorded and
	// one made by hand before the index declared it — which is exactly the
	// state every early adopter of a shortcut is in.
	for _, a := range ownAliases {
		delete(others, a)
	}
	if ownBinary != "" {
		for name, path := range others {
			if sameFile(path, ownBinary) {
				delete(others, name)
			}
		}
	}
	return pluginmgr.CheckAliases(entry, builtinCommandNames(), others)
}

// reconcileAliases brings an installed plugin's alias links in line with the
// index, without reinstalling the binary.
//
// Failures here are reported and swallowed: the plugin itself is installed and
// working under its real name, and a missing shortcut is not worth failing an
// upgrade over.
func reconcileAliases(stdout, stderr io.Writer, entry *pluginmgr.Entry, state *pluginmgr.State) {
	if len(entry.Aliases) == 0 && len(state.Aliases) == 0 {
		return
	}
	if conflicts := aliasConflicts(entry); len(conflicts) > 0 {
		_, _ = fmt.Fprintf(stderr, "⚠️  %v\n", pluginmgr.JoinConflicts(entry.Name, conflicts))
		return
	}
	have, err := pluginmgr.EnsureAliases(state, entry.Aliases, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "⚠️  reconciling aliases for %s: %v\n", entry.Name, err)
		return
	}
	if equalStrings(have, state.Aliases) {
		return
	}
	state.Aliases = have
	if err := pluginmgr.WriteState(state); err != nil {
		_, _ = fmt.Fprintf(stderr, "⚠️  saving plugin state: %v\n", err)
		return
	}
	for _, a := range have {
		_, _ = fmt.Fprintf(stdout, "   also reachable as `viti %s`\n", a)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sameFile reports whether two paths reach the same file once symlinks are
// resolved.
func sameFile(a, b string) bool {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false
	}
	return ra == rb
}

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
		// Refused before anything is downloaded: a taken alias is a decision
		// for whoever curates the index, and finding out after the install
		// would leave a half-configured plugin behind.
		if !installNoAliases {
			if err := pluginmgr.JoinConflicts(entry.Name, aliasConflicts(entry)); err != nil {
				return err
			}
		}
		state, err := pluginmgr.Install(cmd.Context(), entry, pluginmgr.InstallOptions{
			Version:      version,
			Prefix:       installPrefix,
			SkipChecksum: installSkipChecksum,
			SkipCosign:   installSkipCosign,
			SkipAliases:  installNoAliases,
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
		for _, a := range state.Aliases {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "   also reachable as `viti %s`\n", a)
		}
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
	// Deliberately not release.FetchLatest: that talks to GitHub anonymously,
	// which is fine for vitictl's own public repo but returns 404 for a
	// private plugin repo — indistinguishable from "no releases". pluginmgr
	// owns the credentials, so the check has to go through it, exactly like
	// the install below does.
	latestTag, err := pluginmgr.LatestVersion(ctx, entry.Repo)
	if err != nil {
		return fmt.Errorf("checking %s: %w", entry.Repo, err)
	}
	switch release.Compare(state.Version, latestTag) {
	case release.StatusUpToDate:
		// Aliases are reconciled even here. A plugin already on the latest
		// release never reinstalls, so without this an alias added to the
		// index would reach new installs only and never anyone already
		// current — which is most people.
		reconcileAliases(stdout, stderr, entry, state)
		_, _ = fmt.Fprintf(stdout, "✅ %s %s — already up to date\n", state.Name, state.Version)
		return nil
	case release.StatusAhead:
		reconcileAliases(stdout, stderr, entry, state)
		_, _ = fmt.Fprintf(stdout, "🧪 %s %s is ahead of latest (%s) — skipping\n", state.Name, state.Version, latestTag)
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "⬆️  %s: %s -> %s\n", state.Name, state.Version, latestTag)

	// A viti upgrade can add a command that claims an alias installed months
	// ago, so the check runs here too. Unlike install this only warns: the
	// upgrade is wanted regardless, and refusing it over a cosmetic shortcut
	// would strand the plugin on an old version.
	conflicts := aliasConflicts(entry)
	if len(conflicts) > 0 {
		_, _ = fmt.Fprintf(stderr, "⚠️  %v\n", pluginmgr.JoinConflicts(entry.Name, conflicts))
	}
	newState, err := pluginmgr.Install(ctx, entry, pluginmgr.InstallOptions{
		Version:     latestTag,
		Prefix:      dirOf(state.BinaryPath),
		SkipAliases: len(conflicts) > 0,
		Stderr:      stderr,
	})
	if err != nil {
		return err
	}
	// Aliases dropped this round are still ours to clean up; carrying the old
	// list forward would leave state claiming links that no longer exist.
	if len(conflicts) > 0 {
		newState.Aliases = state.Aliases
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
	pluginInstallCmd.Flags().BoolVar(&installNoAliases, "no-aliases", false,
		"install only viti-<name>, skipping the short aliases the index declares")

	pluginCmd.AddCommand(pluginInstallCmd)
	pluginCmd.AddCommand(pluginUpgradeCmd)
	pluginCmd.AddCommand(pluginUninstallCmd)
}
