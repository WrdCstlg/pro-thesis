//go:build !windows

package driver

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func terminateTree(cmd *exec.Cmd, grace time.Duration) error {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return nil
	}
	pid := cmd.Process.Pid
	pgid := -pid

	// Try graceful SIGTERM to process group first
	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}

	if grace <= 0 {
		grace = DefaultGrace
	}

	done := make(chan struct{})
	go func() {
		for {
			if err := syscall.Kill(pid, 0); err != nil && errors.Is(err, syscall.ESRCH) {
				close(done)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	select {
	case <-done:
		return nil
	case <-time.After(grace):
		// Escalate to SIGKILL for process group
		if err := syscall.Kill(pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		return nil
	}
}

func signalOf(err error) (bool, string) {
	if err == nil {
		return false, ""
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if status, ok := ee.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return true, status.Signal().String()
		}
	}
	return false, ""
}
