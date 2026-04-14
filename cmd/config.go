package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl/internal/settings"
)

var configCmd = &cobra.Command{
	Use:     "config",
	Aliases: []string{"c"},
	Short:   "⚙️  Manage viti configuration (~/.vitistack/ctl.config.yaml)",
}

var configInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Interactively add a new availability zone to the viti config file",
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := settings.ConfigFilePath()
		if err != nil {
			return err
		}
		fmt.Printf("⚙️  viti config file: %s\n\n", path)

		reader := bufio.NewReader(cmd.InOrStdin())

		name, err := promptRequired(reader, "Availability zone name (e.g. prod-west): ")
		if err != nil {
			return err
		}
		kc, err := promptOptional(reader, "Path to kubeconfig (leave blank to skip): ")
		if err != nil {
			return err
		}
		ctxName, err := promptOptional(reader, "kubectl context name (leave blank to skip): ")
		if err != nil {
			return err
		}

		z := settings.AvailabilityZone{Name: name, Kubeconfig: kc, Context: ctxName}
		if err := settings.AddAvailabilityZone(z); err != nil {
			return err
		}
		fmt.Printf("\n✅ Saved availability zone %q.\n", name)
		return nil
	},
}

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show the current viti configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := settings.ConfigFilePath()
		if err != nil {
			return err
		}
		fmt.Printf("⚙️  config file: %s\n", path)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			fmt.Println("📭 (file does not exist yet — run `viti config init`)")
			return nil
		}
		zones, err := settings.AvailabilityZones()
		if err != nil {
			return err
		}
		if len(zones) == 0 {
			fmt.Println("📭 no availability zones configured")
			return nil
		}
		tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "NAME\tKUBECONFIG\tCONTEXT")
		for _, z := range zones {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n",
				z.Name, valueOrDash(z.Kubeconfig), valueOrDash(z.Context))
		}
		return tw.Flush()
	},
}

var (
	addKubeconfig string
	addContext    string
)

var configAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Add or update an availability zone",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if addKubeconfig == "" && addContext == "" {
			return fmt.Errorf("provide at least --kubeconfig or --context")
		}
		z := settings.AvailabilityZone{Name: args[0], Kubeconfig: addKubeconfig, Context: addContext}
		if err := settings.AddAvailabilityZone(z); err != nil {
			return err
		}
		fmt.Printf("✅ saved availability zone %q\n", z.Name)
		return nil
	},
}

var configRemoveCmd = &cobra.Command{
	Use:     "remove <name>",
	Aliases: []string{"rm", "delete"},
	Short:   "Remove an availability zone",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := settings.RemoveAvailabilityZone(args[0]); err != nil {
			return err
		}
		fmt.Printf("🗑️  removed availability zone %q\n", args[0])
		return nil
	},
}

var configListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List configured availability zones",
	RunE:    configShowCmd.RunE,
}

func promptRequired(r *bufio.Reader, prompt string) (string, error) {
	for {
		fmt.Print(prompt)
		s, err := r.ReadString('\n')
		if err != nil {
			return "", err
		}
		if s = strings.TrimSpace(s); s != "" {
			return s, nil
		}
		fmt.Println("❌ value is required")
	}
}

func promptOptional(r *bufio.Reader, prompt string) (string, error) {
	fmt.Print(prompt)
	s, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(s), nil
}

func valueOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func init() {
	configAddCmd.Flags().StringVar(&addKubeconfig, "kubeconfig", "", "path to a kubeconfig file")
	configAddCmd.Flags().StringVar(&addContext, "context", "", "name of a kubectl context")
	configCmd.AddCommand(configInitCmd, configShowCmd, configListCmd, configAddCmd, configRemoveCmd)
}
