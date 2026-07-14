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

func TestPidRefCounter_RetainCreatesFile(t *testing.T) {
	rc := NewPidRefCounter(t.TempDir())

	require.NoError(t, rc.Retain())
	t.Cleanup(func() { _ = rc.Release() })

	pidFile := filepath.Join(rc.path, strconv.Itoa(os.Getpid())+".pid")
	_, err := os.Stat(pidFile)
	assert.NoError(t, err, "pid file %q should exist after Retain", pidFile)
}

func TestPidRefCounter_ReleaseRemovesFile(t *testing.T) {
	rc := NewPidRefCounter(t.TempDir())

	require.NoError(t, rc.Retain())
	require.NoError(t, rc.Release())

	pidFile := filepath.Join(rc.path, strconv.Itoa(os.Getpid())+".pid")
	_, err := os.Stat(pidFile)
	assert.True(t, os.IsNotExist(err), "pid file %q should not exist after Release", pidFile)
}

func TestPidRefCounter_References(t *testing.T) {
	rc := NewPidRefCounter(t.TempDir())

	n, err := rc.ReferenceCount()
	require.NoError(t, err)
	assert.Equal(t, 0, n, "initial references")

	require.NoError(t, rc.Retain())
	t.Cleanup(func() { _ = rc.Release() }) // safety net

	n, err = rc.ReferenceCount()
	require.NoError(t, err)
	assert.Equal(t, 1, n, "references after Retain")

	require.NoError(t, rc.Release())

	n, err = rc.ReferenceCount()
	require.NoError(t, err)
	assert.Equal(t, 0, n, "references after Release")
}

func TestPidRefCounter_DeadProcessNotCounted(t *testing.T) {
	dir := t.TempDir()
	rc := NewPidRefCounter(dir)

	// Run a short-lived process and record its PID once it has exited.
	cmd := exec.Command("true")
	require.NoError(t, cmd.Run())
	deadPid := cmd.ProcessState.Pid()
	// Note: PID reuse is theoretically possible but negligibly unlikely here.

	deadPidFile := filepath.Join(dir, strconv.Itoa(deadPid)+".pid")
	require.NoError(t, os.WriteFile(deadPidFile, nil, 0644))

	n, err := rc.ReferenceCount()
	require.NoError(t, err)
	assert.Equal(t, 0, n, "dead process should not count as a reference")
}
