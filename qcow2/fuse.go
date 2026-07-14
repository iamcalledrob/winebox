package qcow2

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/prometheus/procfs"
)

func fuseUnmountAndWait(ctx context.Context, daemon string, mountPoint string) error {
	// A fuse daemon keeps resources busy for a period even after umount returns (although we
	// are not using umount expliitly anymore, keeping comment for history). The mount
	// point gets removed immediately from the kernel mount table, but the associated fuse
	// daemon will stay running until any post-unmount ops are done e.g. flushing changes to disk.
	// Depending on op., the fuse daemon might be fuse2fs or something else. Currently, this
	// module only uses fuse2fs.
	//
	// "Not found" is considered an error: the daemon should be running if a mount is active.
	pid, err := fuseDaemonPID(daemon, mountPoint)
	if err != nil {
		return fmt.Errorf("finding fuse daemon (%s) pid for mount: %w", daemon, err)
	}

	// SIGTERM the fuse daemon rather than calling umount/fusermount3 because some OSs
	// (i.e. NixOS) use a setuid wrapper to allow mounting/unmounting without root, and
	// we experienced issues where the wrapped version was being usurped by the declared
	// pkgs.fusermount3 dependency which was not wrapped. Causing issues unmounting in
	// packaged builds.
	//
	// fuse daemons are designed to gracefully handle a SIGTERM.
	return killAndWait(ctx, pid)

}

func fuseDaemonPID(daemon string, mountpoint string) (int, error) {
	procs, err := findProcessesByName(daemon)
	if err != nil {
		return 0, fmt.Errorf("finding '%s' procs: %w", daemon, err)
	}

	for _, p := range procs {
		args, _ := p.CmdLine()
		if slices.Contains(args, mountpoint) {
			return p.PID, nil
		}
	}
	return 0, fmt.Errorf("not found")
}

func killAndWait(ctx context.Context, pid int) error {
	err := syscall.Kill(pid, syscall.SIGTERM)
	if err != nil {
		return fmt.Errorf("sending SIGTERM to %d: %w", pid, err)
	}

	// Wait for fuse* daemon to exit and thus release resources
	err = waitForExit(ctx, pid)
	if err != nil {
		return fmt.Errorf("waiting for %d to exit: %w", pid, err)
	}

	return nil
}

// isMountPoint reports whether path is a mount point by checking whether it
// resides on a different device from its parent directory.
func isMountPoint(path string) bool {
	var st, parentSt syscall.Stat_t
	if syscall.Lstat(path, &st) != nil {
		return false
	}
	if syscall.Lstat(filepath.Dir(path), &parentSt) != nil {
		return false
	}
	return st.Dev != parentSt.Dev
}

func findProcessesByName(name string) ([]procfs.Proc, error) {
	procs, err := procfs.AllProcs()
	if err != nil {
		return nil, fmt.Errorf("all procs: %w", err)
	}
	return slices.DeleteFunc(procs, func(p procfs.Proc) bool {
		// Don't use p.Comm() because it can get truncated
		path, _ := p.Executable()
		executable := filepath.Base(path)
		return executable != name
	}), nil
}

func waitForExit(ctx context.Context, pid int) error {
	for {
		if !processAlive(pid) {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
