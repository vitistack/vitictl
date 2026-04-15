//go:build windows

package plugin

import (
	"errors"
	"os"
	"os/exec"
)

func execPlugin(path string, argv, env []string) error {
	// #nosec G204 -- launching a user-installed plugin binary resolved via
	// exec.LookPath from the viti-* naming scheme is the purpose of this
	// dispatcher; see plugin.Find / plugin.LongestMatch for the lookup.
	c := exec.Command(path, argv[1:]...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Env = env
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	os.Exit(0)
	return nil
}
