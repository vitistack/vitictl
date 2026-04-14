package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var globalAZ string

var rootCmd = &cobra.Command{
	Use:   "viti",
	Short: "🚀 Vitistack control CLI",
	Long: `🚀 viti is a command-line tool for interacting with one or more
Vitistack installations: inspecting vitistacks, searching for machines and
Kubernetes clusters, and extracting cluster configuration artifacts.

🌐 A Vitistack deployment may span several availability zones (Kubernetes
clusters): configure one or more kubeconfig/context availability zones in
~/.vitistack/ctl.config.yaml and commands will iterate them. Use
-z/--availabilityzone (or --az) to restrict a command to a single zone.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// AvailabilityZone returns the --availabilityzone/--az value set on the
// root command.
func AvailabilityZone() string { return globalAZ }

func Execute() error {
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
