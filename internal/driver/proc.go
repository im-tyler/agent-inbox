package driver

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

// killGrace is how long a cancelled process group gets to end after TERM
// before it is KILLed. CLIs spawn tool subprocesses that can be anywhere in
// their own shutdown; five seconds covers an orderly exit without letting a
// wedged one hold the turn open.
const killGrace = 5 * time.Second

// startProcess starts name in its own process group, with cancellation that
// owns the whole group.
//
// exec.CommandContext's default cancellation signals the direct child only.
// A coding CLI's tool subprocesses are not that child: they survive the kill,
// and one that inherited stdout goes on holding the pipe, so the driver's
// read loop never saw EOF and Wait blocked on it — cancellation that leaves
// the work running and the caller stuck is not cancellation.
//
// The child is made its own group leader (Setpgid), so a negative pid
// addresses every descendant at once. Cancel TERMes the group and escalates
// to KILL after killGrace. WaitDelay additionally bounds Wait when anything
// still holds a pipe after that: the pipes are closed and Wait returns
// instead of blocking on the orphan.
func startProcess(ctx context.Context, name string, arg ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, arg...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil || cmd.Process.Pid <= 0 {
			return nil
		}
		pgid := cmd.Process.Pid
		if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
			return err
		}
		time.AfterFunc(killGrace, func() {
			syscall.Kill(-pgid, syscall.SIGKILL)
		})
		return nil
	}
	cmd.WaitDelay = killGrace + 5*time.Second
	return cmd
}
