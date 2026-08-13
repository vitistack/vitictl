package pluginmgr

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// AliasBinaryName returns the filename an alias is installed under, so that
// viti's ordinary PATH discovery finds it: `viti kv` looks for viti-kv.
func AliasBinaryName(alias string) string {
	name := "viti-" + alias
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// Conflict is an alias that cannot be installed, and why.
type Conflict struct {
	Alias  string
	Reason string
}

func (c Conflict) Error() string { return fmt.Sprintf("alias %q: %s", c.Alias, c.Reason) }

// CheckAliases reports which of an entry's aliases cannot be used.
//
// Two things can claim a name. A built-in command always wins, because viti
// only looks for a plugin when it does not recognise the subcommand — so an
// alias colliding with one is not an error the user ever sees, it is a command
// that silently does something else. Another plugin's name is the same problem
// one layer out. Both are worth refusing at install time, while it is still
// somebody's decision rather than a mystery.
//
// reserved holds viti's own command names and their aliases. others maps every
// other discovered plugin's name to the path it was found at; the entry's own
// name is ignored so reinstalling never conflicts with itself.
func CheckAliases(entry *Entry, reserved map[string]bool, others map[string]string) []Conflict {
	if entry == nil {
		return nil
	}
	var out []Conflict
	for _, a := range entry.Aliases {
		switch {
		case reserved[a]:
			out = append(out, Conflict{a, "viti already has a built-in command by that name"})
		case a == entry.Name:
			// Already refused at index validation; harmless to skip here.
			continue
		default:
			if path, taken := others[a]; taken {
				out = append(out, Conflict{a, fmt.Sprintf("another plugin is installed as %s", path)})
			}
		}
	}
	return out
}

// JoinConflicts renders conflicts as one actionable error.
func JoinConflicts(name string, conflicts []Conflict) error {
	if len(conflicts) == 0 {
		return nil
	}
	parts := make([]string, 0, len(conflicts))
	for _, c := range conflicts {
		parts = append(parts, c.Error())
	}
	sort.Strings(parts)
	return fmt.Errorf(
		"plugin %q declares an alias that is already taken — %s. "+
			"The plugin itself still installs with --no-aliases; the alias needs changing in the index",
		name, strings.Join(parts, "; "))
}

// EnsureAliases creates whichever of the given aliases are missing beside an
// already-installed binary, and reports the set that exists afterwards.
//
// Installing writes the links once, which is not enough: an alias can be added
// to the index long after a plugin was installed, and a plugin already on the
// latest release never reinstalls. Without a reconciliation step a newly
// declared alias would appear for new users and never for existing ones —
// which is most of them. Upgrading converges the links instead.
//
// A link that already exists but resolves elsewhere is left alone and reported;
// callers check for conflicts first, so reaching that means something changed
// underfoot.
func EnsureAliases(state *State, aliases []string, stderr io.Writer) ([]string, error) {
	if state == nil || state.BinaryPath == "" {
		return nil, errors.New("nil state or missing binary path")
	}
	dir, binaryName := filepath.Split(state.BinaryPath)
	prefix := filepath.Clean(dir)

	have := make([]string, 0, len(aliases))
	for _, a := range aliases {
		link := filepath.Join(prefix, AliasBinaryName(a))
		if _, err := os.Lstat(link); err == nil {
			if resolvesTo(link, state.BinaryPath) {
				have = append(have, a)
			} else {
				logf(stderr, "⚠️  leaving %s alone: it is not this plugin's alias", link)
			}
			continue
		}
		if err := linkAlias(prefix, binaryName, a); err != nil {
			logf(stderr, "⚠️  could not install alias %q: %v", a, err)
			continue
		}
		logf(stderr, "aliased %s -> %s", AliasBinaryName(a), binaryName)
		have = append(have, a)
	}
	return have, nil
}

// resolvesTo reports whether link ends up at target once symlinks are followed.
func resolvesTo(link, target string) bool {
	a, err := filepath.EvalSymlinks(link)
	if err != nil {
		return false
	}
	b, err := filepath.EvalSymlinks(target)
	if err != nil {
		return false
	}
	return a == b
}

// linkAlias points prefix/viti-<alias> at the freshly installed binary.
//
// The link is relative — just the target's filename, both living in the same
// directory — so moving or renaming that directory does not strand it.
//
// Windows reserves symlink creation for privileged accounts or developer mode,
// so a failure there falls back to copying the binary. A copy costs disk and
// goes stale on upgrade, which is why it is the fallback rather than the rule;
// Uninstall knows to remove either form.
func linkAlias(prefix, binaryName, alias string) error {
	link := filepath.Join(prefix, AliasBinaryName(alias))

	// Replace whatever is there: an upgrade re-links every time, and a stale
	// copy from a previous fallback must not survive.
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("replacing %s: %w", link, err)
	}
	if err := os.Symlink(binaryName, link); err == nil {
		return nil
	} else if runtime.GOOS != "windows" {
		return fmt.Errorf("linking %s -> %s: %w", link, binaryName, err)
	}
	if err := installFile(filepath.Join(prefix, binaryName), link); err != nil {
		return fmt.Errorf("copying %s to %s: %w", binaryName, link, err)
	}
	return nil
}

// unlinkAlias removes an alias installed beside binaryName.
//
// It refuses to delete anything that is not recognisably ours: a symlink is
// removed only when it points at our binary, and a plain file only when it is
// byte-identical to it. Someone else's viti-kv on the same PATH is their
// business, and uninstalling a plugin must not take it with them.
func unlinkAlias(prefix, binaryName, alias string) error {
	link := filepath.Join(prefix, AliasBinaryName(alias))
	info, err := os.Lstat(link)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(link)
		if err != nil {
			return err
		}
		// Tolerate both the relative form written above and an absolute one
		// written by hand or by an older version.
		if filepath.Base(target) != binaryName {
			return nil
		}
		return os.Remove(link)
	}

	same, err := sameContents(filepath.Join(prefix, binaryName), link)
	if err != nil || !same {
		return nil //nolint:nilerr // an unreadable or differing file is not ours to delete
	}
	return os.Remove(link)
}

// sameContents reports whether two files hash identically.
func sameContents(a, b string) (bool, error) {
	ha, err := sha256File(a)
	if err != nil {
		return false, err
	}
	hb, err := sha256File(b)
	if err != nil {
		return false, err
	}
	return ha == hb, nil
}
