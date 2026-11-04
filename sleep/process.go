package sleep

import (
	"os"
	"slices"
	"strings"

	"github.com/prometheus/procfs"
)

// ProcessFinder returns the pids to pause. Called just before each sleep.
type ProcessFinder func() []int

// ProcessesUsingMount returns pids that may be using mountPoint by inspecting running
// processes on the system.
//
// Note: FUSE daemons serving mountPoint hold /dev/fuse rather than files under it, so
// will not be returned. Which is probably what you want as the premise of this package
// is to gracefully handle sleep for processes *using* a FUSE mount point, not FUSE itself.
//
// Caveat: only finds processes holding something under mountPoint (cwd, open file etc.)
// at the time of the check. A process that just accesses paths, e.g. calling stat(),
// without holding anything open there will be missed.
func ProcessesUsingMount(mountPoint string) ProcessFinder {
	return func() []int {
		procs, err := procfs.AllProcs()
		if err != nil {
			return nil
		}

		var pids []int
		for _, p := range procs {
			if p.PID != os.Getpid() && usesPath(p, mountPoint) {
				pids = append(pids, p.PID)
			}
		}
		return pids
	}
}

// usesPath checks to see whether a process is using the given mountPoint.
// Checks the process's cwd, root, executable, open files and mapped files.
// Checks cheapest first
func usesPath(p procfs.Proc, path string) bool {
	under := func(s string) bool {
		return s == path || strings.HasPrefix(s, path+"/")
	}

	if cwd, err := p.Cwd(); err == nil && under(cwd) {
		return true
	}
	if root, err := p.RootDir(); err == nil && under(root) {
		return true
	}
	if exe, err := p.Executable(); err == nil && under(exe) {
		return true
	}
	if fds, err := p.FileDescriptorTargets(); err == nil && slices.ContainsFunc(fds, under) {
		return true
	}
	if maps, err := p.ProcMaps(); err == nil && slices.ContainsFunc(maps, func(m *procfs.ProcMap) bool {
		return under(m.Pathname)
	}) {
		return true
	}
	return false
}
