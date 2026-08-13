package cmd

import (
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl/internal/plugin"
	"github.com/vitistack/vitictl/internal/pluginmgr"
)

var pluginCmd = &cobra.Command{
	Use:     "plugin",
	Aliases: []string{"plugins"},
	Short:   "🧩 Manage viti plugins (external binaries named viti-*)",
	Long: `🧩 viti discovers external plugins on PATH whose binary name starts
with "viti-". A binary called viti-foo can be invoked as "viti foo [args...]".

When viti receives a subcommand it does not recognise, it looks for a matching
plugin on PATH and execs it. The first binary on PATH wins; subsequent
binaries of the same name are reported as shadowed by "viti plugin list".

Plugins inherit environment variables describing viti's global state, so they
can cooperate without reparsing viti's flags:
  VITI_AVAILABILITYZONE  value of -z/--availabilityzone/--az (if set)
  VITI_CONFIG            path to the active ctl.config.yaml`,
}

var (
	listAvailable bool
	listAll       bool
)

var pluginListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List plugins discovered on PATH",
	RunE: func(cmd *cobra.Command, args []string) error {
		if listAvailable || listAll {
			return listAvailablePlugins(cmd)
		}
		return listInstalledPlugins(cmd)
	},
}

func listInstalledPlugins(cmd *cobra.Command) error {
	found, err := plugin.List()
	if err != nil {
		return err
	}
	if len(found) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(),
			"No viti-* plugins found on PATH. Run `viti plugin list --available` to see installable plugins.")
		return nil
	}

	// Map managed plugins by name so we can report their tracked version.
	managed := map[string]*pluginmgr.State{}
	if states, err := pluginmgr.ListStates(); err == nil {
		for _, s := range states {
			managed[s.Name] = s
		}
	}

	builtins := builtinCommandNames()
	aliasOf := aliasOwners(found, managed)
	seen := make(map[string]string)

	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tVERSION\tPATH\tSTATUS")
	for _, p := range found {
		// An alias is the same install reached by another name, not a second
		// plugin; it is reported on its owner's row instead of claiming one.
		if _, isAlias := aliasOf[p.Name]; isAlias {
			continue
		}
		var notes []string
		if prior, dup := seen[p.Name]; dup {
			notes = append(notes, fmt.Sprintf("shadowed by %s", prior))
		} else {
			seen[p.Name] = p.Path
		}
		if builtins[p.Name] {
			notes = append(notes, "shadowed by built-in command")
		}

		name := p.Name
		if s, ok := managed[p.Name]; ok && len(s.Aliases) > 0 {
			name = fmt.Sprintf("%s (%s)", p.Name, strings.Join(s.Aliases, ", "))
			// viti can grow a command after an alias was installed, which is
			// exactly when a working shortcut goes quietly dead. Re-checking
			// on every listing is the only place that gets caught.
			for _, a := range s.Aliases {
				if builtins[a] {
					notes = append(notes,
						fmt.Sprintf("alias %q shadowed by built-in command", a))
				}
			}
		}

		status := "ok"
		if len(notes) > 0 {
			status = strings.Join(notes, "; ")
		}
		version := "-"
		if s, ok := managed[p.Name]; ok {
			version = s.Version
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", name, version, p.Path, status)
	}
	return tw.Flush()
}

// aliasOwners maps an alias name to the plugin that owns it.
//
// Recorded state is authoritative, but a link made by hand — or by an older
// viti that did not track aliases — is still an alias in every way that
// matters, so binaries resolving to the same file are folded in too.
func aliasOwners(found []plugin.Plugin, managed map[string]*pluginmgr.State) map[string]string {
	out := map[string]string{}
	for name, s := range managed {
		for _, a := range s.Aliases {
			out[a] = name
		}
	}

	real := make(map[string]string, len(found))
	for _, p := range found {
		resolved, err := filepath.EvalSymlinks(p.Path)
		if err != nil {
			continue
		}
		if owner, ok := real[resolved]; ok {
			// Same file under two names: the first discovered keeps the row.
			if _, known := out[p.Name]; !known && p.Name != owner {
				out[p.Name] = owner
			}
			continue
		}
		real[resolved] = p.Name
	}
	return out
}

func listAvailablePlugins(cmd *cobra.Command) error {
	idx, err := pluginmgr.FetchIndex(cmd.Context())
	if err != nil {
		return fmt.Errorf("fetching plugin index: %w", err)
	}
	installed := map[string]*pluginmgr.State{}
	if states, err := pluginmgr.ListStates(); err == nil {
		for _, s := range states {
			installed[s.Name] = s
		}
	}
	listable := idx.Listable(listAll)
	hidden := len(idx.Plugins) - len(listable.Plugins)

	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	header := "NAME\tREPO\tINSTALLED\tDESCRIPTION"
	if listAll {
		header = "NAME\tREPO\tINSTALLED\tPRIVATE\tDESCRIPTION"
	}
	_, _ = fmt.Fprintln(tw, header)
	for _, e := range listable.Plugins {
		ver := "-"
		if s, ok := installed[e.Name]; ok {
			ver = s.Version
		}
		if listAll {
			private := "-"
			if e.Private {
				private = "yes"
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.Name, e.Repo, ver, private, e.Description)
			continue
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.Name, e.Repo, ver, e.Description)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	// Say that something was withheld, so a missing plugin is never a mystery.
	if hidden > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"\n%d plugin(s) not shown (marked private). Use --all to include them.\n", hidden)
	}
	return nil
}

// builtinCommandNames returns the set of subcommand names (and aliases)
// registered on the root command. Used by the dispatcher to know when
// to yield to cobra, and by `plugin list` to flag shadowed plugins.
func builtinCommandNames() map[string]bool {
	out := map[string]bool{
		"help":       true,
		"completion": true,
	}
	for _, c := range rootCmd.Commands() {
		out[c.Name()] = true
		for _, a := range c.Aliases {
			out[a] = true
		}
	}
	return out
}

func init() {
	pluginListCmd.Flags().BoolVar(&listAll, "all", false,
		"include plugins marked private in the index (implies --available)")
	pluginListCmd.Flags().BoolVar(&listAvailable, "available", false,
		"list installable plugins from the curated index instead of installed ones")
	pluginCmd.AddCommand(pluginListCmd)
	rootCmd.AddCommand(pluginCmd)
}
