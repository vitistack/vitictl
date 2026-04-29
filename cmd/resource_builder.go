package cmd

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vitistack/vitictl/internal/fuzzy"
	"github.com/vitistack/vitictl/internal/kube"
	"github.com/vitistack/vitictl/internal/printer"
)

// resourceBinding describes how to list/get/search a single CRD type via
// the generic helper below. T is a pointer to the API object (e.g.
// *MachineProvider); TList is a pointer to its list type.
type resourceBinding[T ctrlclient.Object, TList ctrlclient.ObjectList] struct {
	Use        string   // singular command name, e.g. "machineprovider"
	Aliases    []string // e.g. ["mp", "machineproviders"]
	Short      string   // one-line description for the parent subcommand row
	Namespaced bool     // true if resource is namespace-scoped
	NameKind   string   // kind prefix for -o name output, e.g. "machineprovider"

	NewList     func() TList
	Items       func(TList) []T
	Headers     func(wide bool) string                       // tab-separated header row
	Row         func(azName string, obj T, wide bool) string // tab-separated row
	SearchLabel func(azName string, obj T) string            // label to fuzzy-match against

	// SortKeys are resource-specific sort comparators, keyed by lowercase
	// column name (e.g. "phase", "provider"). The defaults — "name", "az",
	// "age", and "namespace" (when Namespaced) — are added automatically;
	// anything provided here augments or overrides them.
	SortKeys map[string]func(a, b T) int
}

type azItem[T ctrlclient.Object] struct {
	azName string
	obj    T
}

// buildResourceCmd returns a parent cobra command with list/get/search
// subcommands for the bound resource type.
func buildResourceCmd[T ctrlclient.Object, TList ctrlclient.ObjectList](b resourceBinding[T, TList]) *cobra.Command {
	root := &cobra.Command{
		Use:     b.Use,
		Aliases: b.Aliases,
		Short:   b.Short,
	}

	comparators := buildResourceComparators(b)

	var listNS, listOut, listSort string
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List " + b.Use + " across all configured availability zones",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCollect(cmd, listOut, listNS, listSort, b, comparators, func(hits []azItem[T], format printer.Format) error {
				if len(hits) == 0 && !format.IsStructured() {
					fmt.Println("🤷 no " + b.Use + " found")
					return nil
				}
				return renderResource(cmd, hits, format, b)
			})
		},
	}
	listCmd.Flags().StringVarP(&listOut, "output", "o", "", outputFlagHelp)
	listCmd.Flags().StringVarP(&listSort, "sort", "s", "", sortFlagHelpFor(comparators))
	if b.Namespaced {
		listCmd.Flags().StringVarP(&listNS, "namespace", "n", "", "limit to this namespace")
	}

	var getNS, getOut string
	getCmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Show a " + b.Use + " by name (searches all availability zones unless --az)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCollect(cmd, getOut, getNS, "", b, comparators, func(all []azItem[T], format printer.Format) error {
				var hits []azItem[T]
				for _, h := range all {
					if h.obj.GetName() == args[0] {
						hits = append(hits, h)
					}
				}
				if len(hits) == 0 {
					return fmt.Errorf("❌ no %s named %q found on any availability zone", b.Use, args[0])
				}
				return renderResource(cmd, hits, format, b)
			})
		},
	}
	getCmd.Flags().StringVarP(&getOut, "output", "o", "", outputFlagHelp)
	if b.Namespaced {
		getCmd.Flags().StringVarP(&getNS, "namespace", "n", "", "namespace of the resource")
	}

	var searchNS, searchOut, searchSort string
	searchCmd := &cobra.Command{
		Use:   "search [query]",
		Short: "Fuzzy-search " + b.Use + " across all configured availability zones",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCollect(cmd, searchOut, searchNS, "", b, comparators, func(all []azItem[T], format printer.Format) error {
				query := ""
				if len(args) == 1 {
					query = args[0]
				}
				candidates := make([]fuzzy.Candidate[azItem[T]], 0, len(all))
				for _, h := range all {
					candidates = append(candidates, fuzzy.Candidate[azItem[T]]{
						Label: b.SearchLabel(h.azName, h.obj),
						Item:  h,
					})
				}
				matches := fuzzy.Search(query, candidates)
				hits := make([]azItem[T], 0, len(matches))
				for _, m := range matches {
					hits = append(hits, m.Item)
				}
				// --sort overrides fuzzy ranking when explicitly given.
				if err := sortByKeys(hits, searchSort, comparators); err != nil {
					return err
				}
				if len(hits) == 0 && !format.IsStructured() {
					fmt.Println("🤷 no " + b.Use + " matched")
					return nil
				}
				return renderResource(cmd, hits, format, b)
			})
		},
	}
	searchCmd.Flags().StringVarP(&searchOut, "output", "o", "", outputFlagHelp)
	searchCmd.Flags().StringVarP(&searchSort, "sort", "s", "", sortFlagHelpFor(comparators))
	if b.Namespaced {
		searchCmd.Flags().StringVarP(&searchNS, "namespace", "n", "", "limit search to this namespace")
	}

	root.AddCommand(listCmd, getCmd, searchCmd)
	return root
}

const outputFlagHelp = "output format: wide, json, yaml, name (default: table)"

// buildResourceComparators assembles the comparator map a resource list/search
// supports: the built-ins (name, az, age, and optionally namespace) plus any
// resource-specific keys defined on the binding.
func buildResourceComparators[T ctrlclient.Object, TList ctrlclient.ObjectList](
	b resourceBinding[T, TList],
) map[string]func(a, b azItem[T]) int {
	out := map[string]func(a, b azItem[T]) int{
		"name": func(a, b azItem[T]) int { return cmpStrings(a.obj.GetName(), b.obj.GetName()) },
		"az":   func(a, b azItem[T]) int { return cmpStrings(a.azName, b.azName) },
		"age": func(a, b azItem[T]) int {
			// Smallest age first when ascending → newest creationTimestamp first.
			ta := a.obj.GetCreationTimestamp().Time
			tb := b.obj.GetCreationTimestamp().Time
			if ta.Equal(tb) {
				return 0
			}
			if ta.After(tb) {
				return -1
			}
			return 1
		},
	}
	if b.Namespaced {
		out["namespace"] = func(a, b azItem[T]) int { return cmpStrings(a.obj.GetNamespace(), b.obj.GetNamespace()) }
	}
	for k, f := range b.SortKeys {
		f := f
		out[k] = func(a, b azItem[T]) int { return f(a.obj, b.obj) }
	}
	return out
}

// runCollect parses the format, connects to all resolved AZs, collects items
// into a flat azItem slice, applies the optional sort, then hands the slice
// to `act` for filtering/rendering.
func runCollect[T ctrlclient.Object, TList ctrlclient.ObjectList](
	_ *cobra.Command,
	outFlag, nsFlag, sortFlag string,
	b resourceBinding[T, TList],
	comparators map[string]func(a, b azItem[T]) int,
	act func(hits []azItem[T], format printer.Format) error,
) error {
	format, err := printer.Parse(outFlag)
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
	hits := collectResource(ctx, clients, nsFlag, b)
	if err := sortByKeys(hits, sortFlag, comparators); err != nil {
		return err
	}
	return act(hits, format)
}

func collectResource[T ctrlclient.Object, TList ctrlclient.ObjectList](
	ctx context.Context,
	clients []*kube.Client,
	namespace string,
	b resourceBinding[T, TList],
) []azItem[T] {
	var out []azItem[T]
	for _, c := range clients {
		list := b.NewList()
		opts := []ctrlclient.ListOption{}
		if b.Namespaced && namespace != "" {
			opts = append(opts, ctrlclient.InNamespace(namespace))
		}
		if err := c.Ctrl.List(ctx, list, opts...); err != nil {
			warn(fmt.Errorf("availability zone %q: listing %s: %w", c.AZ.Name, b.Use, err))
			continue
		}
		for _, it := range b.Items(list) {
			out = append(out, azItem[T]{azName: c.AZ.Name, obj: it})
		}
	}
	return out
}

func renderResource[T ctrlclient.Object, TList ctrlclient.ObjectList](
	cmd *cobra.Command,
	hits []azItem[T],
	format printer.Format,
	b resourceBinding[T, TList],
) error {
	switch format {
	case printer.FormatJSON, printer.FormatYAML:
		objs := make([]runtime.Object, 0, len(hits))
		for _, h := range hits {
			objs = append(objs, h.obj)
		}
		if format == printer.FormatJSON {
			return printer.WriteJSON(cmd.OutOrStdout(), objs)
		}
		return printer.WriteYAML(cmd.OutOrStdout(), objs)
	case printer.FormatName:
		for _, h := range hits {
			if b.Namespaced {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), b.NameKind+"/"+h.obj.GetNamespace()+"/"+h.obj.GetName())
			} else {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), b.NameKind+"/"+h.obj.GetName())
			}
		}
		return nil
	case printer.FormatWide:
		return writeResourceTable(cmd, hits, true, b)
	default:
		return writeResourceTable(cmd, hits, false, b)
	}
}

func writeResourceTable[T ctrlclient.Object, TList ctrlclient.ObjectList](
	cmd *cobra.Command,
	hits []azItem[T],
	wide bool,
	b resourceBinding[T, TList],
) error {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, b.Headers(wide))
	for _, h := range hits {
		_, _ = fmt.Fprintln(tw, b.Row(h.azName, h.obj, wide))
	}
	return tw.Flush()
}

// itemsOf is a tiny convenience used by resource bindings to convert a list's
// []T value slice to []*T so the generic helper can work with ctrlclient.Object.
func itemsOf[T any](items []T) []*T {
	out := make([]*T, len(items))
	for i := range items {
		out[i] = &items[i]
	}
	return out
}
