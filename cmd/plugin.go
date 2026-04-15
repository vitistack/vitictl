package cmd

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl/internal/plugin"
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

var pluginListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List plugins discovered on PATH",
	RunE: func(cmd *cobra.Command, args []string) error {
		found, err := plugin.List()
		if err != nil {
			return err
		}
		if len(found) == 0 {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No viti-* plugins found on PATH.")
			return nil
		}

		builtins := builtinCommandNames()
		seen := make(map[string]string)

		tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "NAME\tPATH\tSTATUS")
		for _, p := range found {
			var notes []string
			if prior, dup := seen[p.Name]; dup {
				notes = append(notes, fmt.Sprintf("shadowed by %s", prior))
			} else {
				seen[p.Name] = p.Path
			}
			if builtins[p.Name] {
				notes = append(notes, "shadowed by built-in command")
			}
			status := "ok"
			if len(notes) > 0 {
				status = strings.Join(notes, "; ")
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, p.Path, status)
		}
		return tw.Flush()
	},
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
	pluginCmd.AddCommand(pluginListCmd)
	rootCmd.AddCommand(pluginCmd)
}
