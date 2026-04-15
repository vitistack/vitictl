//go:build !windows

package plugin

import "syscall"

func execPlugin(path string, argv, env []string) error {
	// #nosec G204 -- launching a user-installed plugin binary resolved via
	// exec.LookPath from the viti-* naming scheme is the purpose of this
	// dispatcher; see plugin.Find / plugin.LongestMatch for the lookup.
	return syscall.Exec(path, argv, env)
}
