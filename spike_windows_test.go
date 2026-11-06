//go:build windows

// Spike for #26: start a child suspended, assign it to a Job Object, then
// resume it. It uses x/sys/windows directly, not procman, so it tests only the
// mechanism. This file is not meant to be merged.
package procman_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const spikeSpawnNowMode = "--procman-spike-spawn-now"

func init() {
	if len(os.Args) < 3 || os.Args[1] != spikeSpawnNowMode {
		return
	}
	// Spawn a grandchild as the first thing the child does, then stay alive.
	self, err := os.Executable()
	if err != nil {
		os.Exit(2)
	}
	gc := exec.Command(self, testSleepMode, "30")
	if err := gc.Start(); err != nil {
		os.Exit(3)
	}
	_ = os.WriteFile(os.Args[2], []byte(strconv.Itoa(gc.Process.Pid)), 0o600)
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

var procIsProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

func spikeInJob(t *testing.T, pid int, job windows.Handle) bool {
	t.Helper()
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		t.Fatalf("OpenProcess(%d): %v", pid, err)
	}
	defer windows.CloseHandle(h)
	var result int32
	r, _, callErr := procIsProcessInJob.Call(uintptr(h), uintptr(job), uintptr(unsafe.Pointer(&result)))
	if r == 0 {
		t.Fatalf("IsProcessInJob(%d): %v", pid, callErr)
	}
	return result != 0
}

func spikeNewJob(t *testing.T) windows.Handle {
	t.Helper()
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatalf("CreateJobObject: %v", err)
	}
	// Kill-on-close, so a failed run leaves nothing behind in the job.
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		t.Fatalf("SetInformationJobObject: %v", err)
	}
	t.Cleanup(func() { windows.CloseHandle(job) })
	return job
}

func spikeAssign(t *testing.T, job windows.Handle, pid int) {
	t.Helper()
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		t.Fatalf("OpenProcess for assignment: %v", err)
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		t.Fatalf("AssignProcessToJobObject: %v", err)
	}
}

// spikeResume resumes every thread of pid. It returns how many threads it
// found and the previous suspend count of each.
func spikeResume(pid int) (prev []uint32, err error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return nil, fmt.Errorf("CreateToolhelp32Snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snap, &entry); err == nil; err = windows.Thread32Next(snap, &entry) {
		if entry.OwnerProcessID != uint32(pid) {
			continue
		}
		th, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return prev, fmt.Errorf("OpenThread(%d): %w", entry.ThreadID, err)
		}
		n, err := windows.ResumeThread(th)
		windows.CloseHandle(th)
		if err != nil {
			return prev, fmt.Errorf("ResumeThread(%d): %w", entry.ThreadID, err)
		}
		prev = append(prev, n)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return prev, fmt.Errorf("Thread32Next: %w", err)
	}
	return prev, nil
}

func spikeReadPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err == nil {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("grandchild PID never written to %s", path)
	return 0
}

// The window between creation and assignment is widened to 200ms so the
// outcome is deterministic: unsuspended, the grandchild must escape the job;
// suspended, it must be inside.
func TestSpikeSuspendedStartContainsEarlyGrandchild(t *testing.T) {
	for _, suspended := range []bool{false, true} {
		t.Run(fmt.Sprintf("suspended=%v", suspended), func(t *testing.T) {
			job := spikeNewJob(t)
			self, _ := os.Executable()
			pidFile := filepath.Join(t.TempDir(), "gc.pid")
			cmd := exec.Command(self, spikeSpawnNowMode, pidFile)
			cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
			if suspended {
				cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
			}
			t0 := time.Now()
			if err := cmd.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Logf("cmd.Start returned after %v", time.Since(t0))
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

			time.Sleep(200 * time.Millisecond)
			spikeAssign(t, job, cmd.Process.Pid)
			if suspended {
				t1 := time.Now()
				prev, err := spikeResume(cmd.Process.Pid)
				t.Logf("resume: %v, threads found %d, previous suspend counts %v", time.Since(t1), len(prev), prev)
				if err != nil {
					t.Fatalf("resume: %v", err)
				}
				if len(prev) != 1 || prev[0] != 1 {
					t.Errorf("want exactly one thread with suspend count 1, got %v", prev)
				}
			}
			gc := spikeReadPid(t, pidFile)
			if !suspended {
				defer func() {
					if p, err := os.FindProcess(gc); err == nil {
						_ = p.Kill()
					}
				}()
			}
			childIn := spikeInJob(t, cmd.Process.Pid, job)
			gcIn := spikeInJob(t, gc, job)
			t.Logf("child in job: %v, grandchild in job: %v", childIn, gcIn)
			if !childIn {
				t.Errorf("child not in job")
			}
			if gcIn != suspended {
				t.Errorf("grandchild in job = %v, want %v", gcIn, suspended)
			}
		})
	}
}

// The failure path: a process that is never resumed must still be killable
// and reapable.
func TestSpikeSuspendedProcessCanBeKilledAndReaped(t *testing.T) {
	self, _ := os.Executable()
	cmd := exec.Command(self, testSleepMode, "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		t.Logf("Wait returned: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return for a killed suspended process")
	}
}

// Cost of one system-wide thread snapshot walk, which the resume pays per start.
func TestSpikeThreadSnapshotCost(t *testing.T) {
	const n = 50
	threads := 0
	t0 := time.Now()
	for i := 0; i < n; i++ {
		snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
		count := 0
		for err = windows.Thread32First(snap, &entry); err == nil; err = windows.Thread32Next(snap, &entry) {
			count++
		}
		windows.CloseHandle(snap)
		threads = count
	}
	t.Logf("snapshot+walk: %v average over %d runs, %d threads on the system", time.Since(t0)/n, n, threads)
}
