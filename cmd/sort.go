package cmd

import (
	"fmt"
	"sort"
	"strings"
)

// sortKey is a single parsed component of a --sort flag value, e.g.
// "-age" parses to {name: "age", desc: true}.
type sortKey struct {
	name string
	desc bool
}

// parseSortSpec splits a comma-separated sort spec like "az,name,-age"
// into ordered sortKey components. Empty input returns nil, nil.
func parseSortSpec(s string) ([]sortKey, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]sortKey, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		desc := false
		switch p[0] {
		case '-':
			desc = true
			p = p[1:]
		case '+':
			p = p[1:]
		}
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("empty sort key in spec %q", s)
		}
		out = append(out, sortKey{name: strings.ToLower(p), desc: desc})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty sort spec")
	}
	return out, nil
}

// sortByKeys applies the parsed keys from spec to hits in order using the
// provided comparator map. Comparator keys must be lowercase. An unknown
// sort key returns an error listing the available keys. A nil/empty spec
// is a no-op.
func sortByKeys[H any](hits []H, spec string, comparators map[string]func(a, b H) int) error {
	keys, err := parseSortSpec(spec)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	cmps := make([]func(a, b H) int, 0, len(keys))
	for _, k := range keys {
		f, ok := comparators[k.name]
		if !ok {
			return fmt.Errorf("unknown sort key %q (available: %s)", k.name, sortedKeyList(comparators))
		}
		if k.desc {
			inner := f
			f = func(a, b H) int { return -inner(a, b) }
		}
		cmps = append(cmps, f)
	}
	sort.SliceStable(hits, func(i, j int) bool {
		for _, c := range cmps {
			if v := c(hits[i], hits[j]); v != 0 {
				return v < 0
			}
		}
		return false
	})
	return nil
}

// sortedKeyList returns the comparator keys as a sorted, comma-separated
// string suitable for help text and error messages.
func sortedKeyList[H any](comparators map[string]func(a, b H) int) string {
	keys := make([]string, 0, len(comparators))
	for k := range comparators {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// sortFlagHelpFor builds the help string for a --sort flag listing the
// keys the caller's comparator map supports. The returned string has the
// form: "sort by columns (comma-separated, prefix '-' for desc). Available: ...".
func sortFlagHelpFor[H any](comparators map[string]func(a, b H) int) string {
	return "sort by columns (comma-separated, prefix '-' for desc). Available: " + sortedKeyList(comparators)
}

// cmpStrings is the standard ascending string comparator.
func cmpStrings(a, b string) int { return strings.Compare(a, b) }

// cmpBool orders false before true (ascending).
func cmpBool(a, b bool) int {
	switch {
	case !a && b:
		return -1
	case a && !b:
		return 1
	}
	return 0
}
