package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// gitCommand runs git in its own process group, kills the whole group on
// cancellation (a configured helper — fsmonitor, credential, filter — is a
// git child, and killing only git leaves it holding the output pipe), and
// bounds the wait on inherited pipes for a descendant that escapes the
// group. Unix-only, like the rest of this program.
func gitCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	return cmd
}
