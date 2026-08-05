//go:build windows

package procman

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type platformState struct {
	mu  sync.Mutex
	job windows.Handle
}

// SupportsParentDeathCleanup reports whether native parent-death cleanup is
// available on the current platform.
func SupportsParentDeathCleanup() bool {
	return true
}

func prepareExecCmd(child *ExecCmd) error {
	if child.cmd.SysProcAttr == nil {
		child.cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	child.cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
	if !child.processTreeTermination && !child.parentDeathCleanup {
		return nil
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create Job Object: %w", err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	if child.parentDeathCleanup {
		limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		uint32(unsafe.Sizeof(limits)),
	); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("configure Job Object: %w", err)
	}
	child.platform.mu.Lock()
	child.platform.job = job
	child.platform.mu.Unlock()
	return nil
}

func finishExecCmdStart(child *ExecCmd) error {
	child.platform.mu.Lock()
	defer child.platform.mu.Unlock()
	if child.platform.job == 0 {
		return nil
	}
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(child.cmd.Process.Pid),
	)
	if err != nil {
		return fmt.Errorf("open process for Job Object: %w", err)
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(child.platform.job, process); err != nil {
		return fmt.Errorf("assign process to Job Object: %w", err)
	}
	return nil
}

func cleanupExecCmd(child *ExecCmd) {
	child.platform.mu.Lock()
	defer child.platform.mu.Unlock()
	if child.platform.job != 0 {
		_ = windows.CloseHandle(child.platform.job)
		child.platform.job = 0
	}
}

func gracefulSignal(child *ExecCmd) error {
	// CTRL_BREAK_EVENT to the child's process group (PID == group ID with CREATE_NEW_PROCESS_GROUP).
	err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(child.cmd.Process.Pid))
	return err
}

func forceKill(child *ExecCmd) error {
	if child.processTreeTermination {
		child.platform.mu.Lock()
		defer child.platform.mu.Unlock()
		if child.platform.job == 0 {
			return nil
		}
		return windows.TerminateJobObject(child.platform.job, 1)
	}
	err := child.cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func processIsAlive(proc *os.Process) bool {
	const stillActive = 259 // STILL_ACTIVE
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(proc.Pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
