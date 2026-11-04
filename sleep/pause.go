package sleep

import (
	"fmt"
	"syscall"
	"time"

	"github.com/prometheus/procfs"
)

// stopTimeout bounds waiting for processes to stop before letting sleep proceed.
// logind's InhibitDelayMaxSec (default 5s) caps the wait regardless.
const stopTimeout = 3 * time.Second

// PauseProcessesDuringSleep pauses (SIGSTOP) the processes returned by the find callback
// before the system sleeps, and continues them (SIGCONT) on wake.
func PauseProcessesDuringSleep(label InhibitLabel, find ProcessFinder) (cancel func(), err error) {
	var paused []int
	beforeSleep := func() {
		paused = StopAndWait(find(), stopTimeout)
	}
	afterWake := func() {
		Continue(paused)
		paused = nil
	}

	var stop func()
	stop, err = Listen(label, beforeSleep, afterWake)
	if err != nil {
		err = fmt.Errorf("listening for sleep events: %w", err)
		return
	}

	cancel = func() {
		stop()
		Continue(paused) // never leave processes paused behind us
	}
	return
}

// StopAndWait SIGSTOPs pids and waits (up to timeout) until all their threads have
// actually stopped. A thread blocked in a FUSE request only stops once the request
// completes, which is quick while the FUSE daemons are still running.
// Returns the pids that were signalled.
func StopAndWait(pids []int, timeout time.Duration) []int {
	var stopped []int
	for _, pid := range pids {
		if syscall.Kill(pid, syscall.SIGSTOP) == nil {
			stopped = append(stopped, pid)
		}
	}

	deadline := time.Now().Add(timeout)
	for _, pid := range stopped {
		for !isStopped(pid) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
	return stopped
}

func Continue(pids []int) {
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGCONT)
	}
}

// isStopped reports whether every thread of pid is stopped (or the process has gone).
func isStopped(pid int) bool {
	threads, err := procfs.AllThreads(pid)
	if err != nil {
		return true
	}

	var stat procfs.ProcStat
	for _, t := range threads {
		stat, err = t.Stat()
		if err == nil && stat.State != "T" {
			return false
		}
	}
	return true
}
