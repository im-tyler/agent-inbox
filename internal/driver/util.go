package driver

import (
	"crypto/rand"
	"fmt"
	"os/exec"
	"strings"
)

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// wrapExec surfaces a child process's stderr on failure instead of the opaque
// "exit status N".
func wrapExec(err error) error {
	if ee, ok := err.(*exec.ExitError); ok {
		msg := strings.TrimSpace(string(ee.Stderr))
		if msg == "" {
			return fmt.Errorf("exit %d", ee.ExitCode())
		}
		return fmt.Errorf("exit %d: %s", ee.ExitCode(), msg)
	}
	return err
}
