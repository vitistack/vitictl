package printer

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

// Format is the selected output encoding for a list/get command.
type Format string

const (
	FormatTable Format = ""
	FormatWide  Format = "wide"
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml"
	FormatName  Format = "name"
)

// ValidFormats is the list of formats accepted by -o. The empty string (the
// default table view) is valid but omitted from this list for flag help.
var ValidFormats = []string{"wide", "json", "yaml", "name"}

// Parse validates and normalizes a raw -o flag value.
func Parse(s string) (Format, error) {
	switch strings.ToLower(s) {
	case "", "table":
		return FormatTable, nil
	case "wide":
		return FormatWide, nil
	case "json":
		return FormatJSON, nil
	case "yaml", "yml":
		return FormatYAML, nil
	case "name":
		return FormatName, nil
	default:
		return FormatTable, fmt.Errorf("unsupported output format %q (valid: %s)", s, strings.Join(ValidFormats, ", "))
	}
}

// IsStructured reports whether this format is machine-readable (JSON/YAML),
// in which case the caller should suppress emoji/decorative stdout and
// hand rendering off to WriteJSON/WriteYAML.
func (f Format) IsStructured() bool {
	return f == FormatJSON || f == FormatYAML
}

// WriteJSON serializes objs as a single object (len==1) or a k8s-style List
// envelope. Each object must already be a pointer to a typed API object
// with populated TypeMeta (GVK), otherwise kubectl-compatible output
// cannot preserve kind information.
func WriteJSON(w io.Writer, objs []runtime.Object) error {
	if len(objs) == 1 {
		data, err := json.MarshalIndent(objs[0], "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(data))
		return err
	}
	data, err := json.MarshalIndent(listEnvelope(objs), "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}

// WriteYAML serializes objs as a single object or a k8s-style List envelope.
func WriteYAML(w io.Writer, objs []runtime.Object) error {
	if len(objs) == 1 {
		data, err := yaml.Marshal(objs[0])
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	data, err := yaml.Marshal(listEnvelope(objs))
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func listEnvelope(objs []runtime.Object) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "List",
		"items":      objs,
	}
}

// Age formats a creation timestamp as a short duration string (e.g. "3d",
// "7h", "5m") relative to now. Returns "-" for zero timestamps.
func Age(t metav1.Time) string {
	if t.IsZero() {
		return "-"
	}
	return shortDuration(time.Since(t.Time))
}

func shortDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	default:
		return fmt.Sprintf("%dy", int(d.Hours())/(24*365))
	}
}
