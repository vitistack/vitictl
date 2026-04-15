package cmd

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/fuzzy"
	"github.com/vitistack/vitictl/internal/kube"
	"github.com/vitistack/vitictl/internal/printer"
)

var machineCmd = &cobra.Command{
	Use:     "machine",
	Aliases: []string{"m", "machines"},
	Short:   "🖥️  Work with Machine resources",
}

var (
	machineListNamespace   string
	machineListOutput      string
	machineGetNamespace    string
	machineGetOutput       string
	machineSearchNamespace string
	machineSearchOutput    string
)

type machineHit struct {
	azName  string
	client  *kube.Client
	machine *vitiv1alpha1.Machine
}

func collectMachines(
	ctx context.Context,
	clients []*kube.Client,
	namespace string,
) []machineHit {
	var hits []machineHit
	for _, c := range clients {
		var list vitiv1alpha1.MachineList
		opts := []ctrlclient.ListOption{}
		if namespace != "" {
			opts = append(opts, ctrlclient.InNamespace(namespace))
		}
		if err := c.Ctrl.List(ctx, &list, opts...); err != nil {
			warn(fmt.Errorf("availability zone %q: listing machines: %w", c.AZ.Name, err))
			continue
		}
		for i := range list.Items {
			hits = append(hits, machineHit{azName: c.AZ.Name, client: c, machine: &list.Items[i]})
		}
	}
	return hits
}

var machineListCmd = &cobra.Command{
	Use:   "list",
	Short: "List Machines across all configured availability zones",
	RunE: func(cmd *cobra.Command, args []string) error {
		format, err := printer.Parse(machineListOutput)
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
		hits := collectMachines(ctx, clients, machineListNamespace)
		if len(hits) == 0 && !format.IsStructured() {
			fmt.Println("🤷 no machines found")
			return nil
		}
		return renderMachines(cmd, hits, format)
	},
}

var machineGetCmd = &cobra.Command{
	Use:   "get <name>",
	Short: "Show details of a Machine (searches all availability zones unless --az is given)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		format, err := printer.Parse(machineGetOutput)
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
		all := collectMachines(ctx, clients, machineGetNamespace)
		var hits []machineHit
		for _, h := range all {
			if h.machine.Name == args[0] {
				hits = append(hits, h)
			}
		}
		if len(hits) == 0 {
			return fmt.Errorf("❌ no machine named %q found on any availability zone", args[0])
		}
		if format == printer.FormatTable {
			for i, h := range hits {
				if i > 0 {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), strings.Repeat("-", 60))
				}
				printMachine(cmd, h.azName, h.machine)
			}
			if len(hits) > 1 {
				fmt.Printf("\n🔎 %q found on %d availability zones / namespaces\n", args[0], len(hits))
			}
			return nil
		}
		return renderMachines(cmd, hits, format)
	},
}

var machineSearchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Fuzzy-search Machines across all configured availability zones",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		format, err := printer.Parse(machineSearchOutput)
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
		all := collectMachines(ctx, clients, machineSearchNamespace)

		query := ""
		if len(args) == 1 {
			query = args[0]
		}
		candidates := make([]fuzzy.Candidate[machineHit], 0, len(all))
		for _, h := range all {
			label := strings.Join([]string{
				h.azName, h.machine.Namespace, h.machine.Name,
				h.machine.Status.MachineID, h.machine.Status.ProviderID, h.machine.Status.Hostname,
			}, " ")
			candidates = append(candidates, fuzzy.Candidate[machineHit]{Label: label, Item: h})
		}
		matches := fuzzy.Search(query, candidates)
		hits := make([]machineHit, 0, len(matches))
		for _, m := range matches {
			hits = append(hits, m.Item)
		}
		if len(hits) == 0 && !format.IsStructured() {
			fmt.Println("🤷 no machines matched")
			return nil
		}
		return renderMachines(cmd, hits, format)
	},
}

func renderMachines(cmd *cobra.Command, hits []machineHit, format printer.Format) error {
	switch format {
	case printer.FormatJSON, printer.FormatYAML:
		objs := make([]runtime.Object, 0, len(hits))
		for _, h := range hits {
			objs = append(objs, h.machine)
		}
		if format == printer.FormatJSON {
			return printer.WriteJSON(cmd.OutOrStdout(), objs)
		}
		return printer.WriteYAML(cmd.OutOrStdout(), objs)
	case printer.FormatName:
		for _, h := range hits {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "machine/"+h.machine.Namespace+"/"+h.machine.Name)
		}
		return nil
	case printer.FormatWide:
		return writeMachineTable(cmd, hits, true)
	default:
		return writeMachineTable(cmd, hits, false)
	}
}

func writeMachineTable(cmd *cobra.Command, hits []machineHit, wide bool) error {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if wide {
		_, _ = fmt.Fprintln(tw, "AZ\tNAMESPACE\tNAME\tPROVIDER\tPHASE\tIPS\tMACHINE ID\tHOSTNAME\tARCH\tOS\tCPU\tMEMORY\tAGE")
	} else {
		_, _ = fmt.Fprintln(tw, "AZ\tNAMESPACE\tNAME\tPROVIDER\tPHASE\tIPS\tMACHINE ID")
	}
	for _, h := range hits {
		m := h.machine
		ips := strings.Join(allIPs(m), ",")
		if wide {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
				h.azName, m.Namespace, m.Name,
				string(m.Spec.Provider), m.Status.Phase,
				ips, m.Status.MachineID,
				valueOrDash(m.Status.Hostname),
				valueOrDash(m.Status.Architecture),
				valueOrDash(m.Status.OperatingSystem),
				m.Spec.CPU.Cores,
				humanBytes(m.Spec.Memory),
				printer.Age(m.CreationTimestamp),
			)
		} else {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				h.azName, m.Namespace, m.Name,
				string(m.Spec.Provider), m.Status.Phase,
				truncate(ips, 40), m.Status.MachineID,
			)
		}
	}
	return tw.Flush()
}

func printMachine(cmd *cobra.Command, azName string, m *vitiv1alpha1.Machine) {
	out := cmd.OutOrStdout()
	pf := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }
	pl := func(s string) { _, _ = fmt.Fprintln(out, s) }

	pf("🎯 AZ:            %s\n", azName)
	pf("🏷️  Name:          %s\n", m.Name)
	pf("📦 Namespace:     %s\n", m.Namespace)
	pf("☁️  Provider:      %s\n", string(m.Spec.Provider))
	pf("🧩 MachineClass:  %s\n", m.Spec.MachineClass)
	pf("🚦 Phase:         %s %s\n", phaseEmoji(m.Status.Phase), m.Status.Phase)
	if m.Status.State != "" {
		pf("📊 State:         %s\n", m.Status.State)
	}
	if m.Status.Hostname != "" {
		pf("🪪 Hostname:      %s\n", m.Status.Hostname)
	}
	if m.Status.Architecture != "" || m.Status.OperatingSystem != "" {
		pf("🧱 Arch / OS:     %s / %s\n", valueOrDash(m.Status.Architecture), valueOrDash(m.Status.OperatingSystem))
	}
	if m.Spec.CPU.Cores > 0 || m.Spec.Memory > 0 {
		pf("🧠 CPU / Memory:  %d cores / %s\n", m.Spec.CPU.Cores, humanBytes(m.Spec.Memory))
	}
	pf("⏱️  Age:           %s\n", printer.Age(m.CreationTimestamp))

	if ips := allIPs(m); len(ips) > 0 {
		pl("\n🌐 IP Addresses:")
		for _, ip := range ips {
			pf("  - %s\n", ip)
		}
	}
	if len(m.Status.PublicIPAddresses) > 0 {
		pl("\n🌍 Public IPs:")
		for _, ip := range m.Status.PublicIPAddresses {
			pf("  - %s\n", ip)
		}
	}
	if m.Status.MachineID != "" || m.Status.ProviderID != "" {
		pl("\n🆔 Identifiers:")
		if m.Status.MachineID != "" {
			pf("  - machineID:  %s\n", m.Status.MachineID)
		}
		if m.Status.ProviderID != "" {
			pf("  - providerID: %s\n", m.Status.ProviderID)
		}
	}
}

func allIPs(m *vitiv1alpha1.Machine) []string {
	out := make([]string, 0, len(m.Status.IPAddresses)+len(m.Status.PublicIPAddresses))
	out = append(out, m.Status.IPAddresses...)
	out = append(out, m.Status.PublicIPAddresses...)
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// humanBytes renders a byte count as a short, human-readable string such as
// "4Gi" or "512Mi". Returns "-" for zero.
func humanBytes(b int64) string {
	if b <= 0 {
		return "-"
	}
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
		tib = 1024 * gib
	)
	switch {
	case b >= tib:
		return fmt.Sprintf("%dTi", b/tib)
	case b >= gib:
		return fmt.Sprintf("%dGi", b/gib)
	case b >= mib:
		return fmt.Sprintf("%dMi", b/mib)
	case b >= kib:
		return fmt.Sprintf("%dKi", b/kib)
	default:
		return fmt.Sprintf("%dB", b)
	}
}

func init() {
	machineListCmd.Flags().StringVarP(&machineListNamespace, "namespace", "n", "", "limit to this namespace")
	machineListCmd.Flags().StringVarP(&machineListOutput, "output", "o", "",
		"output format: wide, json, yaml, name (default: table)")

	machineGetCmd.Flags().StringVarP(&machineGetNamespace, "namespace", "n", "", "namespace of the Machine")
	machineGetCmd.Flags().StringVarP(&machineGetOutput, "output", "o", "",
		"output format: wide, json, yaml, name (default: detailed view)")

	machineSearchCmd.Flags().StringVarP(&machineSearchNamespace, "namespace", "n", "", "limit search to this namespace")
	machineSearchCmd.Flags().StringVarP(&machineSearchOutput, "output", "o", "",
		"output format: wide, json, yaml, name (default: table)")

	machineCmd.AddCommand(machineListCmd, machineGetCmd, machineSearchCmd)
}
