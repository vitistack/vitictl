package cmd

import (
	"os"
	"strings"

	"github.com/vitistack/vitictl/internal/plugin"
	"github.com/vitistack/vitictl/internal/settings"
)

// valueTakingGlobalFlags lists the global flags that consume the next
// argument as their value when written in non-equals form. Keep this in
// sync with the flags registered in root.go's init().
var valueTakingGlobalFlags = map[string]bool{
	"-z":                 true,
	"--availabilityzone": true,
	"--az":               true,
}

// maybeDispatchPlugin inspects os.Args[1:] and, if the invocation does not
// match a built-in subcommand but does match a viti-* plugin on PATH,
// replaces the current process with the plugin. It returns (true, err) if
// it attempted a dispatch, in which case the caller should return err
// without invoking cobra. (false, nil) means "let cobra handle it".
func maybeDispatchPlugin() (bool, error) {
	args := os.Args[1:]
	if len(args) == 0 {
		return false, nil
	}

	tokens, firstIdx := collectPluginTokens(args)
	if len(tokens) == 0 {
		return false, nil
	}

	// If the first candidate is a built-in command, let cobra handle it —
	// even if a same-named plugin also happens to exist on PATH. This
	// matches kubectl's behaviour: built-ins always win.
	if builtinCommandNames()[tokens[0]] {
		return false, nil
	}

	path, matched, ok := plugin.LongestMatch(tokens)
	if !ok {
		return false, nil
	}

	pluginArgs := argsAfterMatch(args, firstIdx, len(matched))
	env := buildPluginEnv(args)
	return true, plugin.Exec(path, pluginArgs, env)
}

// collectPluginTokens returns the sequence of consecutive non-flag tokens
// that could form a plugin name, along with the index of the first such
// token in args. Value-taking global flags written in space-separated form
// (e.g. "-z prod") have their value skipped so it isn't mistaken for a
// command.
func collectPluginTokens(args []string) (tokens []string, firstIdx int) {
	firstIdx = -1
	i := 0
	for i < len(args) {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			// Skip the flag. If it's a known value-taking flag without
			// an embedded '=', also skip the next arg (its value).
			if !strings.Contains(a, "=") && valueTakingGlobalFlags[a] && i+1 < len(args) {
				i += 2
				continue
			}
			i++
			continue
		}
		firstIdx = i
		break
	}
	if firstIdx < 0 {
		return nil, -1
	}
	for j := firstIdx; j < len(args); j++ {
		if strings.HasPrefix(args[j], "-") {
			break
		}
		tokens = append(tokens, args[j])
	}
	return tokens, firstIdx
}

// argsAfterMatch returns the slice of args that should be forwarded to the
// plugin. It preserves original order and keeps every flag that appeared
// before the matched tokens (plugins may care about global flags) — only
// the matched tokens themselves are dropped.
func argsAfterMatch(args []string, firstIdx, matchedLen int) []string {
	out := make([]string, 0, len(args)-matchedLen)
	out = append(out, args[:firstIdx]...)
	out = append(out, args[firstIdx+matchedLen:]...)
	return out
}

// buildPluginEnv extends os.Environ() with viti-specific variables so
// plugins can read global state without reparsing flags.
func buildPluginEnv(args []string) []string {
	env := os.Environ()
	if az := extractGlobalFlagValue(args, "-z", "--availabilityzone", "--az"); az != "" {
		env = append(env, "VITI_AVAILABILITYZONE="+az)
	}
	if cfg, err := settings.ConfigFilePath(); err == nil {
		env = append(env, "VITI_CONFIG="+cfg)
	}
	return env
}

// extractGlobalFlagValue scans args for any of the given flag names in
// either "--name=value" or "--name value" form and returns the first
// value found, or "" if none.
func extractGlobalFlagValue(args []string, names ...string) string {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if eq := strings.IndexByte(a, '='); eq > 0 {
			if want[a[:eq]] {
				return a[eq+1:]
			}
			continue
		}
		if want[a] && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
