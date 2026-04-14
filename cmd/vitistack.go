package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/kube"
	"github.com/vitistack/vitictl/internal/printer"
)

var vitistackCmd = &cobra.Command{
	Use:     "vitistack",
	Aliases: []string{"vs", "vitistacks"},
	Short:   "🌐 Inspect Vitistack resources",
}

var vitistackListOutput string

var vitistackListCmd = &cobra.Command{
	Use:   "list",
	Short: "List Vitistacks across all configured availability zones",
	RunE: func(cmd *cobra.Command, args []string) error {
		format, err := printer.Parse(vitistackListOutput)
		if err != nil {
			return err
		}
		ctx := context.Background()
		zones, err := kube.ResolveAvailabilityZones(AvailabilityZone())
		if err != nil {
			return err
		}
		clients, err := kube.ConnectAll(ctx, zones, true, warn)
		if err != nil {
			return err
		}

		var hits []vitistackHit
		for _, c := range clients {
			var list vitiv1alpha1.VitistackList
			if err := c.Ctrl.List(ctx, &list); err != nil {
				warn(fmt.Errorf("availability zone %q: listing vitistacks: %w", c.AZ.Name, err))
				continue
			}
			for i := range list.Items {
				hits = append(hits, vitistackHit{azName: c.AZ.Name, vs: &list.Items[i]})
			}
		}

		if len(hits) == 0 && !format.IsStructured() {
			fmt.Println("🤷 no vitistacks found")
			return nil
		}
		return renderVitistacks(cmd, hits, format)
	},
}

var vitistackGetOutput string

var vitistackGetCmd = &cobra.Command{
	Use:   "get <name>",
	Short: "Show details of a Vitistack (searches all availability zones unless --az is given)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		format, err := printer.Parse(vitistackGetOutput)
		if err != nil {
			return err
		}
		ctx := context.Background()
		zones, err := kube.ResolveAvailabilityZones(AvailabilityZone())
		if err != nil {
			return err
		}
		clients, err := kube.ConnectAll(ctx, zones, true, warn)
		if err != nil {
			return err
		}

		var hits []vitistackHit
		for _, c := range clients {
			var v vitiv1alpha1.Vitistack
			err := c.Ctrl.Get(ctx, ctrlclient.ObjectKey{Name: args[0]}, &v)
			if err == nil {
				v := v
				hits = append(hits, vitistackHit{azName: c.AZ.Name, vs: &v})
			}
		}
		if len(hits) == 0 {
			return fmt.Errorf("❌ no vitistack named %q found on any availability zone", args[0])
		}

		// Default "get" renders the emoji-decorated details view; all other
		// formats use the shared renderer.
		if format == printer.FormatTable {
			for i, h := range hits {
				if i > 0 {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), strings.Repeat("-", 60))
				}
				printVitistack(cmd, h.azName, h.vs)
			}
			if len(hits) > 1 {
				fmt.Printf("\n🔎 %q found on %d availability zones\n", args[0], len(hits))
			}
			return nil
		}
		return renderVitistacks(cmd, hits, format)
	},
}

type vitistackHit struct {
	azName string
	vs     *vitiv1alpha1.Vitistack
}

func renderVitistacks(cmd *cobra.Command, hits []vitistackHit, format printer.Format) error {
	switch format {
	case printer.FormatJSON, printer.FormatYAML:
		objs := make([]runtime.Object, 0, len(hits))
		for _, h := range hits {
			objs = append(objs, h.vs)
		}
		if format == printer.FormatJSON {
			return printer.WriteJSON(cmd.OutOrStdout(), objs)
		}
		return printer.WriteYAML(cmd.OutOrStdout(), objs)
	case printer.FormatName:
		for _, h := range hits {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "vitistack/"+h.vs.Name)
		}
		return nil
	case printer.FormatWide:
		return writeVitistackTable(cmd, hits, true)
	default:
		return writeVitistackTable(cmd, hits, false)
	}
}

func writeVitistackTable(cmd *cobra.Command, hits []vitistackHit, wide bool) error {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if wide {
		_, _ = fmt.Fprintln(tw, "AZ\tNAME\tDISPLAY NAME\tREGION\tZONE\tINFRA\tPHASE\tDESCRIPTION\t#MP\t#KP\tAGE")
	} else {
		_, _ = fmt.Fprintln(tw, "AZ\tNAME\tDISPLAY NAME\tREGION\tZONE\tINFRA\tPHASE")
	}
	for _, h := range hits {
		v := h.vs
		if wide {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n",
				h.azName, v.Name, v.Spec.DisplayName,
				v.Spec.Region, v.Spec.Zone, v.Spec.Infrastructure, v.Status.Phase,
				truncate(v.Spec.Description, 40),
				len(v.Spec.MachineProviders), len(v.Spec.KubernetesProviders),
				printer.Age(v.CreationTimestamp),
			)
		} else {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				h.azName, v.Name, v.Spec.DisplayName,
				v.Spec.Region, v.Spec.Zone, v.Spec.Infrastructure, v.Status.Phase,
			)
		}
	}
	return tw.Flush()
}

func printVitistack(cmd *cobra.Command, azName string, v *vitiv1alpha1.Vitistack) {
	out := cmd.OutOrStdout()
	pf := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }
	pl := func(s string) { _, _ = fmt.Fprintln(out, s) }

	pf("🎯 AZ:             %s\n", azName)
	pf("🏷️  Name:           %s\n", v.Name)
	pf("📝 Display Name:   %s\n", v.Spec.DisplayName)
	pf("📄 Description:    %s\n", v.Spec.Description)
	pf("🌍 Region:         %s\n", v.Spec.Region)
	pf("📍 Zone:           %s\n", v.Spec.Zone)
	pf("🏗️  Infrastructure: %s\n", v.Spec.Infrastructure)
	pf("🚦 Phase:          %s %s\n", phaseEmoji(v.Status.Phase), v.Status.Phase)
	pf("⏱️  Age:            %s\n", printer.Age(v.CreationTimestamp))

	if len(v.Spec.MachineProviders) > 0 {
		pl("\n🖥️  Machine Providers:")
		for _, p := range v.Spec.MachineProviders {
			pf("  - %s/%s (priority=%d, enabled=%v)\n", p.Namespace, p.Name, p.Priority, p.Enabled)
		}
	}
	if len(v.Spec.KubernetesProviders) > 0 {
		pl("\n☸️  Kubernetes Providers:")
		for _, p := range v.Spec.KubernetesProviders {
			pf("  - %s/%s (priority=%d, enabled=%v)\n", p.Namespace, p.Name, p.Priority, p.Enabled)
		}
	}
	if len(v.Status.Conditions) > 0 {
		pl("\n📋 Conditions:")
		for _, c := range v.Status.Conditions {
			pf("  - %s=%s  %s\n", c.Type, string(c.Status), strings.TrimSpace(c.Message))
		}
	}
}

// phaseEmoji returns a matching glyph for common phase strings.
func phaseEmoji(phase string) string {
	switch strings.ToLower(phase) {
	case "ready", "running":
		return "🟢"
	case "provisioning", "initializing", "updating", "creating":
		return "🟡"
	case "failed", "degraded":
		return "🔴"
	case "deleting", "terminating", "stopping":
		return "🟠"
	case "stopped", "terminated":
		return "⚫"
	default:
		return "⚪"
	}
}

// warn prints a non-fatal error to stderr so a single unreachable zone
// doesn't abort commands that aggregate across all zones.
func warn(err error) {
	_, _ = fmt.Fprintf(os.Stderr, "⚠️  warning: %v\n", err)
}

func init() {
	vitistackListCmd.Flags().StringVarP(&vitistackListOutput, "output", "o", "",
		"output format: wide, json, yaml, name (default: table)")
	vitistackGetCmd.Flags().StringVarP(&vitistackGetOutput, "output", "o", "",
		"output format: wide, json, yaml, name (default: detailed view)")
	vitistackCmd.AddCommand(vitistackListCmd, vitistackGetCmd)
}
