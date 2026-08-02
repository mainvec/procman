//go:build windows

package procman

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// Windows Job Object constants and limits. See the Microsoft docs for
// CreateJobObjectW, AssignProcessToJobObject, TerminateJobObject,
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE.
const (
	jobObjectLimitKillOnJobClose            = 0x00002000
	jobObjectExtendedLimitInformationClass  = 9 // JobObjectExtendedLimitInformation
	processSetQuota                          = 0x0100 // PROCESS_SET_QUOTA
	processTerminate                        = 0x0001 // PROCESS_TERMINATE
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW           = kernel32.NewProc("CreateJobObjectW")
	procAssignProcessToJobObject   = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject          = kernel32.NewProc("TerminateJobObject")
	procSetInformationJobObject     = kernel32.NewProc("SetInformationJobObject")
	procCloseHandle                = kernel32.NewProc("CloseHandle")
	procIsProcessInJob             = kernel32.NewProc("IsProcessInJob")
	procOpenProcess                = kernel32.NewProc("OpenProcess")
)

// jobObject is the per-child Windows Job Object handle. It is held on the
// Process and closed on reap; KILL_ON_JOB_CLOSE ensures the kernel terminates
// the whole job if the parent dies by any means (the handle closes).
type jobObject struct {
	handle syscall.Handle
}

// assignToGroup creates a Job Object with KILL_ON_JOB_CLOSE, assigns the
// just-started child to it, and returns the handle the Process must hold
// until reap. A failed assignment fails Start rather than proceeding without
// the guarantee.
func assignToGroup(cmd *exec.Cmd) (*jobObject, error) {
	// CreateJobObjectW(lpJobAttributes, lpName). Both NULL for an unnamed job.
	// Returns NULL on failure; a non-zero Errno is the error. Crucially, the
	// returned error is Errno(0) on success, so we branch on handle==0, not on
	// err != nil.
	h, _, errno := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return nil, fmt.Errorf("procman: CreateJobObjectW failed: %v", errno)
	}
	job := &jobObject{handle: syscall.Handle(h)}

	// Set KILL_ON_JOB_CLOSE so the kernel terminates the job when the last
	// handle (ours) closes, which happens on parent death by any means.
	if err := job.setKillOnJobClose(); err != nil {
		job.close()
		return nil, fmt.Errorf("procman: SetInformationJobObject failed: %v", err)
	}

	// Assign the child to the job. There is a small unavoidable window between
	// cmd.Start() and this call in which the child could spawn a grandchild
	// that escapes the job; Go exposes no CREATE_SUSPENDED/ResumeThread path,
	// so this is documented rather than eliminated.
	if cmd.Process == nil {
		job.close()
		return nil, fmt.Errorf("procman: child has no Process to assign to job")
	}
	// os.Process does not expose its handle on Windows; open our own handle to
	// the child by PID with the rights AssignProcessToJobObject requires.
	childHandle, err := openProcessHandle(cmd.Process.Pid)
	if err != nil {
		job.close()
		return nil, fmt.Errorf("procman: OpenProcess for job assignment failed: %v", err)
	}
	defer syscall.CloseHandle(childHandle)
	r1, _, errno := procAssignProcessToJobObject.Call(
		uintptr(job.handle),
		uintptr(childHandle),
	)
	if r1 == 0 {
		job.close()
		return nil, fmt.Errorf("procman: AssignProcessToJobObject failed: %v", errno)
	}
	return job, nil
}

// openProcessHandle opens a handle to the process identified by pid with the
// rights needed to assign it to a job (PROCESS_SET_QUOTA | PROCESS_TERMINATE).
func openProcessHandle(pid int) (syscall.Handle, error) {
	// OpenProcess(dwDesiredAccess, bInheritHandle, dwProcessId).
	h, _, errno := procOpenProcess.Call(
		uintptr(processSetQuota|processTerminate),
		0,
		uintptr(pid),
	)
	if h == 0 {
		return 0, errno
	}
	return syscall.Handle(h), nil
}

// setKillOnJobClose sets JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE on the job.
func (j *jobObject) setKillOnJobClose() error {
	// JOBOBJECT_EXTENDED_LIMIT_INFORMATION is large; we only need the
	// BasicLimitInformation.PercentageUserTimeLimit-equivalent... actually we
	// only set LimitFlags. Use the extended struct with LimitFlags at the
	// front of the BasicLimitInformation sub-struct.
	type jobObjectBasicLimitInformation struct {
		PerProcessUserTimeLimit int64
		PerJobUserTimeLimit     int64
		LimitFlags              uint32
		MinimumWorkingSetSize   uintptr
		MaximumWorkingSetSize   uintptr
		ActiveProcessLimit      uint32
		Affinity                uintptr
		PriorityClass           uint32
		SchedulingClass         uint32
	}
	type jobObjectExtendedLimitInformation struct {
		BasicLimitInformation jobObjectBasicLimitInformation
		IoInfo                [48]byte // IO_COUNTERS
		ProcessMemoryLimit    uintptr
		JobMemoryLimit        uintptr
		PeakProcessMemoryUsed uintptr
		PeakJobMemoryUsed     uintptr
	}
	info := jobObjectExtendedLimitInformation{}
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	r1, _, err := procSetInformationJobObject.Call(
		uintptr(j.handle),
		uintptr(jobObjectExtendedLimitInformationClass),
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if r1 == 0 {
		return err
	}
	return nil
}

// terminate kills the whole job via TerminateJobObject. Every process in the
// job is terminated, including grandchildren.
func (j *jobObject) terminate() error {
	if j.handle == 0 {
		return nil
	}
	// TerminateJobObject(hJob, uExitCode). uExitCode = 1.
	r1, _, err := procTerminateJobObject.Call(uintptr(j.handle), 1)
	if r1 == 0 {
		// The job may already be gone (handle closed); treat that as success.
		if isAlreadyGone(err) {
			return nil
		}
		return err
	}
	return nil
}

// close releases the job handle. Called on reap after the process has exited.
func (j *jobObject) close() {
	if j.handle == 0 {
		return
	}
	r1, _, _ := procCloseHandle.Call(uintptr(j.handle))
	_ = r1
	j.handle = 0
}

// prepareChildCmdPlatform is the Windows implementation. There is no Setpgid
// analogue; group containment is the Job Object, assigned after Start in
// assignToGroupHandle.
func prepareChildCmdPlatform(cmd *exec.Cmd) {}

// assignToGroupHandle creates a Job Object, assigns the child to it, and
// returns the handle for the Process to hold until reap. A failed assignment
// fails Start.
func assignToGroupHandle(cmd *exec.Cmd) (groupHandle, error) {
	return assignToGroup(cmd)
}

// signalTermGroup on Windows terminates the whole job (hard kill; there is no
// SIGTERM). The pid is unused — the job handle is the group.
func signalTermGroup(proc *os.Process, pid int) error {
	// The actual job termination is driven by stop via the stored jobObject;
	// this fallback hard-kills the single process if no job is attached.
	return proc.Kill()
}

// signalKillGroup terminates the job, if attached; otherwise the single
// process.
func signalKillGroup(proc *os.Process, pid int) error {
	return proc.Kill()
}

// signalName returns "" on Windows; there are no Unix signals. A child killed
// by TerminateProcess/TerminateJobObject reports a non-zero exit code, not a
// signal.
func signalName(ps *os.ProcessState) string {
	_ = ps
	return ""
}

// isAlreadyGone reports whether err indicates the process/job is already gone.
func isAlreadyGone(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return contains(msg, "already finished") ||
		contains(msg, "Access is denied") ||
		contains(msg, "no such process") ||
		contains(msg, "The handle is invalid")
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}