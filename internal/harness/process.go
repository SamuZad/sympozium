package harness

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// UseProcessGroup runs cmd as the leader of its own process group so the
// shim can signal everything the wrapped CLI spawned — shells it backgrounded,
// `sympozium-tool exec` clients still waiting on a sidecar, MCP helpers —
// not just the CLI process itself. On context cancellation the group gets
// SIGTERM so the CLI can flush its transcript and telemetry; after waitDelay
// os/exec escalates to SIGKILL of the leader, and KillProcessGroup sweeps up
// anything that outlived it.
//
// Call before cmd.Start.
func UseProcessGroup(cmd *exec.Cmd, waitDelay time.Duration) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return signalGroup(cmd, syscall.SIGTERM) }
	cmd.WaitDelay = waitDelay
}

// KillProcessGroup SIGKILLs cmd's process group. Safe to call after Wait:
// the leader is gone by then but orphaned descendants still carry its pgid.
func KillProcessGroup(cmd *exec.Cmd) {
	_ = signalGroup(cmd, syscall.SIGKILL)
}

func signalGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil || cmd.Process.Pid <= 0 {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
