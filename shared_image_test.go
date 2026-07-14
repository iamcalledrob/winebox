package winebox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockImage struct {
	path         string
	mountCalls   int
	unmountCalls int
}

func (m *mockImage) Mount() error   { m.mountCalls++; return nil }
func (m *mockImage) Unmount() error { m.unmountCalls++; return nil }
func (m *mockImage) Path() string   { return m.path }

func newTestSharedImage(t *testing.T) (*SharedImage, *mockImage) {
	t.Helper()
	orig := AppRuntimeDir
	AppRuntimeDir = filepath.Join(t.TempDir(), "winebox")
	t.Cleanup(func() { AppRuntimeDir = orig })

	mock := &mockImage{path: filepath.Join(t.TempDir(), "mock.qcow2")}
	return NewSharedImage(mock), mock
}

// plantRef writes a .pid file for the given PID into the ref counter directory,
// simulating another process holding a reference. Returns the pid file path so the
// caller can remove it early if needed; t.Cleanup handles removal otherwise.
func plantRef(t *testing.T, si *SharedImage, pid int) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(si.rc.path, 0755))
	pidFile := filepath.Join(si.rc.path, strconv.Itoa(pid)+".pid")
	require.NoError(t, os.WriteFile(pidFile, nil, 0644))
	t.Cleanup(func() { _ = os.Remove(pidFile) })
	return pidFile
}

// startLongRunningProc starts a subprocess we own so we can use its PID as a live
// reference. Using a subprocess rather than e.g. PID 1 avoids EPERM from Signal(0)
// on processes owned by a different user, which processAlive would misread as dead.
func startLongRunningProc(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

// Ensures Mount() only calls through to the underlying Image when no other process
// holds a reference — guarding against redundant double-mounts when multiple processes
// share the same resource.
func TestSharedImage_MountOnlyWhenNecessary(t *testing.T) {
	si, mock := newTestSharedImage(t)
	t.Cleanup(func() { _ = si.Unmount() })

	// With no prior refs, Mount() must call through to the underlying Image.
	require.NoError(t, si.Mount())
	assert.Equal(t, 1, mock.mountCalls, "first Mount() should call raw.Mount() once")
	require.NoError(t, si.Unmount())

	// With an existing live reference (another process already mounted), Mount() must
	// skip raw.Mount().
	plantRef(t, si, startLongRunningProc(t))
	require.NoError(t, si.Mount())
	assert.Equal(t, 1, mock.mountCalls, "Mount() with existing ref should not call raw.Mount() again")
}

// Ensures Unmount() only calls through to the underlying Image when releasing the
// last live reference — guarding against premature teardown while other processes
// still hold the resource.
func TestSharedImage_UnmountOnlyWhenNecessary(t *testing.T) {
	si, mock := newTestSharedImage(t)

	require.NoError(t, si.Mount())

	// Add a second live reference to simulate another process.
	refFile := plantRef(t, si, startLongRunningProc(t))

	// Our Unmount() drops our ref, but the other process still holds one —
	// raw.Unmount() must not fire.
	require.NoError(t, si.Unmount())
	assert.Equal(t, 0, mock.unmountCalls, "Unmount() with remaining refs should not call raw.Unmount()")

	// Remove the other reference now (t.Cleanup would be too late — the sleep process
	// is still alive for the rest of this function), then mount fresh as the sole holder.
	require.NoError(t, os.Remove(refFile))

	// Once we are the last holder, Unmount() must call through to raw.Unmount().
	require.NoError(t, si.Mount())
	require.NoError(t, si.Unmount())
	assert.Equal(t, 1, mock.unmountCalls, "Unmount() as last reference holder should call raw.Unmount()")
}

// Ensures OnWillUnmount fires exactly when the last reference is released.
func TestSharedImage_OnWillUnmount(t *testing.T) {
	si, _ := newTestSharedImage(t)

	fired := 0
	si.OnWillUnmount(func() { fired++ })

	require.NoError(t, si.Mount())

	refFile := plantRef(t, si, startLongRunningProc(t))

	// Still a second holder — callback must not fire.
	require.NoError(t, si.Unmount())
	assert.Equal(t, 0, fired, "OnWillUnmount should not fire while another ref exists")

	require.NoError(t, os.Remove(refFile))

	// Now the sole holder — callback must fire on unmount.
	require.NoError(t, si.Mount())
	require.NoError(t, si.Unmount())
	assert.Equal(t, 1, fired, "OnWillUnmount should fire when last ref is released")
}
