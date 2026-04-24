package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/vitistack/vitictl/internal/release"
)

var globalAZ string

const rootLongBase = `🚀 viti is a command-line tool for interacting with one or more
Vitistack installations: inspecting vitistacks, searching for machines and
Kubernetes clusters, and extracting cluster configuration artifacts.

🌐 A Vitistack deployment may span several availability zones (Kubernetes
clusters): configure one or more kubeconfig/context availability zones in
~/.vitistack/ctl.config.yaml and commands will iterate them. Use
-z/--availabilityzone (or --az) to restrict a command to a single zone.`

var rootCmd = &cobra.Command{
	Use:           "viti",
	Short:         "🚀 Vitistack control CLI",
	Long:          rootLongBase,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// AvailabilityZone returns the --availabilityzone/--az value set on the
// root command.
func AvailabilityZone() string { return globalAZ }

// SetVersion wires the binary's version string into cobra. Called once
// from main() with the -ldflags-injected value. Powers both
// `viti --version` and the `viti version` subcommand. The version is
// also appended to the root command's Long description so it appears
// at the top of `viti --help`.
func SetVersion(v string) {
	if v == "" {
		v = "dev"
	}
	rootCmd.Version = v
	rootCmd.SetVersionTemplate("viti version {{.Version}}\n")
	rootCmd.Long = rootLongBase + "\n\nInstalled version: " + v
}

var versionCheck bool

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the viti version",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		out := cmd.OutOrStdout()
		_, _ = fmt.Fprintf(out, "viti version %s\n", rootCmd.Version)
		if !versionCheck {
			return nil
		}
		return printReleaseCheck(cmd.Context(), out, rootCmd.Version)
	},
}

// printReleaseCheck queries GitHub for the latest release and reports
// whether the locally installed build is up to date. A network failure
// is reported but is not fatal — `viti version --check` should never
// exit non-zero just because the user is offline.
func printReleaseCheck(ctx context.Context, out io.Writer, local string) error {
	latest, err := release.FetchLatest(ctx, release.Repo)
	if err != nil {
		_, _ = fmt.Fprintf(out, "⚠️  could not check for updates: %v\n", err)
		return nil
	}
	status := release.Compare(local, latest.Tag)
	switch status {
	case release.StatusUpToDate:
		_, _ = fmt.Fprintf(out, "✅ you are on the latest release (%s)\n", latest.Tag)
	case release.StatusOutdated:
		_, _ = fmt.Fprintf(out, "🆕 a newer release is available: %s (you have %s)\n", latest.Tag, local)
		_, _ = fmt.Fprintf(out, "   release notes: %s\n", latest.URL)
		_, _ = fmt.Fprintf(out, "   upgrade with:  %s\n", release.UpgradeHint())
		_, _ = fmt.Fprintln(out, "   or run:        viti upgrade")
	case release.StatusAhead:
		_, _ = fmt.Fprintf(out, "🧪 your build (%s) is ahead of the latest release (%s)\n", local, latest.Tag)
	case release.StatusDevelopment:
		_, _ = fmt.Fprintf(out, "🛠  development build (%s); latest release is %s\n", local, latest.Tag)
		_, _ = fmt.Fprintf(out, "   release notes: %s\n", latest.URL)
	}
	return nil
}

func Execute() error {
	if handled, err := maybeDispatchPlugin(); handled {
		if err != nil {
			_, _ = fmt.Fprintln(rootCmd.ErrOrStderr(), "❌ Error:", err)
		}
		return err
	}
	if err := rootCmd.Execute(); err != nil {
		_, _ = fmt.Fprintln(rootCmd.ErrOrStderr(), "❌ Error:", err)
		return err
	}
	return nil
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&globalAZ, "availabilityzone", "z", "",
		"restrict to a single configured availability zone (by name)")
	// --az is an alias for --availabilityzone and writes to the same variable.
	// pflag requires short flags to be a single ASCII character, so "-az"
	// cannot exist as a POSIX short — use "-z" or "--az" instead.
	rootCmd.PersistentFlags().StringVar(&globalAZ, "az", "",
		"alias for --availabilityzone")

	versionCmd.Flags().BoolVar(&versionCheck, "check", false,
		"check GitHub for a newer release and print upgrade instructions")
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(vitistackCmd)
	rootCmd.AddCommand(machineCmd)
	rootCmd.AddCommand(kubernetesClusterCmd)
	rootCmd.AddCommand(machineProviderCmd)
	rootCmd.AddCommand(kubernetesProviderCmd)
	rootCmd.AddCommand(machineClassCmd)
	rootCmd.AddCommand(networkNamespaceCmd)
	rootCmd.AddCommand(networkConfigurationCmd)
	rootCmd.AddCommand(controlPlaneVirtualSharedIPCmd)
	rootCmd.AddCommand(etcdBackupCmd)
	rootCmd.AddCommand(kubevirtConfigCmd)
	rootCmd.AddCommand(proxmoxConfigCmd)

	cobra.AddTemplateFunc("cmdLabel", cmdLabel)
	cobra.AddTemplateFunc("cmdLabelPadding", cmdLabelPadding)
	rootCmd.SetUsageTemplate(usageTemplateWithAliases)
}

// cmdLabel renders a command's name with its aliases parenthesised, e.g.
// "machine (m, machines)". Used in the root usage template so the parent's
// "Available Commands" listing reveals each subcommand's aliases.
func cmdLabel(c *cobra.Command) string {
	if len(c.Aliases) == 0 {
		return c.Name()
	}
	return c.Name() + " (" + strings.Join(c.Aliases, ", ") + ")"
}

// cmdLabelPadding returns the column width needed to align Short
// descriptions when rendering subcommands with their aliases.
func cmdLabelPadding(cmds []*cobra.Command) int {
	max := 0
	for _, c := range cmds {
		if !c.IsAvailableCommand() && c.Name() != "help" {
			continue
		}
		if n := len(cmdLabel(c)); n > max {
			max = n
		}
	}
	return max
}

// usageTemplateWithAliases is Cobra's default usage template with the
// "Available Commands" section modified to use cmdLabel/cmdLabelPadding so
// each subcommand row reads "name (alias1, alias2)  Short description".
const usageTemplateWithAliases = `Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}

Available Commands:{{$cmds := .Commands}}{{$pad := cmdLabelPadding $cmds}}{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad (cmdLabel .) $pad}}  {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`
