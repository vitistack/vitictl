package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl/internal/login"
)

var (
	kubeconfigMergeTarget  string
	talosconfigMergeTarget string
)

var kubeconfigCmd = &cobra.Command{
	Use:   "kubeconfig",
	Short: "🔑 Work with your local kubeconfig",
}

var kubeconfigMergeCmd = &cobra.Command{
	Use:   "merge <file|->",
	Short: "Merge a kubeconfig someone handed you into your own",
	Long: `Merges every cluster, user and context in <file> into your kubeconfig
($KUBECONFIG's first entry, else ~/.kube/config), keeping their names.

Entries with the same name are REPLACED by the incoming ones, so a
re-fetched kubeconfig heals an expired one. (Plain kubectl merging keeps
the old entry.) Your current context is left alone, and the previous file
is kept as <config>.bak-<timestamp>.

Use - to read from stdin, e.g. a kubeconfig pasted into chat:

  pbpaste | viti kubeconfig merge -

For clusters viti can see, 'viti kc login' fetches and merges in one step.`,
	Example: `  viti kubeconfig merge ~/Downloads/bgo-mgmt-001.yaml
  pbpaste | viti kubeconfig merge -`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runConfigMerge(cmd, args[0], kubeconfigMergeTarget, login.MergeKubeconfigVerbatim)
	},
}

var talosconfigCmd = &cobra.Command{
	Use:   "talosconfig",
	Short: "🔑 Work with your local talosconfig",
}

var talosconfigMergeCmd = &cobra.Command{
	Use:   "merge <file|->",
	Short: "Merge a talosconfig someone handed you into your own",
	Long: `Merges every context in <file> into your talosconfig ($TALOSCONFIG,
else ~/.talos/config), keeping their names.

Contexts with the same name are REPLACED by the incoming ones. (talosctl
config merge instead adds a renamed copy.) Your current context is left
alone, and the previous file is kept as <config>.bak-<timestamp>.

Use - to read from stdin.`,
	Example: `  viti talosconfig merge ./talosconfig
  pbpaste | viti talosconfig merge -`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runConfigMerge(cmd, args[0], talosconfigMergeTarget, login.MergeTalosconfigVerbatim)
	},
}

// runConfigMerge reads src ("-" for stdin), merges it into target ("" for the
// tool's default file) and prints what changed.
func runConfigMerge(cmd *cobra.Command, src, target string, merge func([]byte, string) (*login.MergeReport, error)) error {
	var data []byte
	var err error
	if src == "-" {
		data, err = io.ReadAll(cmd.InOrStdin())
	} else {
		data, err = os.ReadFile(src) // #nosec G304 -- the file the user named
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", src, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return fmt.Errorf("%s is empty", src)
	}

	rep, err := merge(data, target)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	replaced := 0
	for _, c := range rep.Changes {
		verb := "➕ added"
		if c.Replaced {
			verb = "🔁 replaced"
			replaced++
		}
		_, _ = fmt.Fprintf(out, "%s %s %s\n", verb, c.Kind, c.Name)
	}
	_, _ = fmt.Fprintf(out, "✅ merged into %s — %d replaced, %d added\n",
		tildePath(rep.Path), replaced, len(rep.Changes)-replaced)
	if rep.Backup != "" {
		_, _ = fmt.Fprintf(out, "   backup: %s\n", tildePath(rep.Backup))
	}
	if rep.CurrentContext != "" {
		_, _ = fmt.Fprintf(out, "   current context: %s\n", rep.CurrentContext)
	}
	return nil
}

func tildePath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.Join("~", rel)
	}
	return p
}

func init() {
	kubeconfigMergeCmd.Flags().StringVar(&kubeconfigMergeTarget, "kubeconfig", "",
		"kubeconfig to merge into (default: $KUBECONFIG's first entry, else ~/.kube/config)")
	kubeconfigCmd.AddCommand(kubeconfigMergeCmd)

	talosconfigMergeCmd.Flags().StringVar(&talosconfigMergeTarget, "talosconfig", "",
		"talosconfig to merge into (default: $TALOSCONFIG, else ~/.talos/config)")
	talosconfigCmd.AddCommand(talosconfigMergeCmd)
}
