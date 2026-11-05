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

func startProcess(child *ExecCmd) error {
	return child.cmd.Start()
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

// gracefulTreeSignal reports that a tree whose direct child has been reaped
// cannot be signalled gracefully. CTRL_BREAK_EVENT addresses a console process
// group by its leader's PID, and Windows reuses PIDs freely once the leader is
// gone. Such a tree is terminated through its Job Object instead.
const gracefulTreeSignal = false

// jobBasicAccounting mirrors JOBOBJECT_BASIC_ACCOUNTING_INFORMATION, which
// golang.org/x/sys/windows does not define.
type jobBasicAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// treeAlive reports whether the command's Job Object still holds a running
// process. Only process-tree termination keeps the job open past the direct
// child: with parent-death cleanup alone, the reaper closes it as before and
// kill-on-close ends any descendants. The job handle, unlike a PID, cannot be
// reused while procman holds it.
func treeAlive(child *ExecCmd) bool {
	if !child.processTreeTermination {
		return false
	}
	child.platform.mu.Lock()
	defer child.platform.mu.Unlock()
	if child.platform.job == 0 {
		return false
	}
	var info jobBasicAccounting
	if err := windows.QueryInformationJobObject(
		child.platform.job,
		windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
		nil,
	); err != nil {
		return false
	}
	return info.ActiveProcesses > 0
}

// signalTree terminates the command's Job Object after its direct child has
// been reaped. graceful is never true here; see gracefulTreeSignal.
func signalTree(child *ExecCmd, graceful bool) error {
	if graceful {
		return nil
	}
	child.platform.mu.Lock()
	defer child.platform.mu.Unlock()
	if child.platform.job == 0 {
		return nil
	}
	return windows.TerminateJobObject(child.platform.job, 1)
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
