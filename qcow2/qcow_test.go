package qcow2

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestBaseImage creates a qcow2 image with an ext4 filesystem formatted inside it.
func newTestBaseImage(t *testing.T) string {
	t.Helper()
	imagePath := filepath.Join(t.TempDir(), "base.qcow2")
	require.NoError(t, CreateBaseImage(imagePath, CompressionTypeNone, 100<<20))

	rawMP, err := mkTemp(filepath.Base(imagePath) + "-raw-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(rawMP) })

	return imagePath
}

// Ensures base image creation works and yields a usable image with the correct configuration
// -- i.e. maxSize and compressionType
//
// Ensures that the image has the expected ext4 filesystem
func TestCreateBaseImage(t *testing.T) {
	require.NoError(t, EnsureDependencies())

	imagePath := filepath.Join(t.TempDir(), "base.qcow2")
	const maxSize = 100 << 20 // 100 MiB

	require.NoError(t, CreateBaseImage(imagePath, CompressionTypeZstd, maxSize))

	_, err := os.Stat(imagePath)
	require.NoError(t, err, "image file should exist")

	// The qcow2 container itself is tiny; only virtual size matches maxSize.
	info, err := os.Stat(imagePath)
	require.NoError(t, err)
	assert.Less(t, info.Size(), int64(maxSize), "qcow2 file should be much smaller than virtual size")

	out, err := exec.Command(qemuImg, "info", "--output=json", imagePath).Output()
	require.NoError(t, err)

	var imgInfo struct {
		Format         string `json:"format"`
		VirtualSize    int64  `json:"virtual-size"`
		FormatSpecific struct {
			Data struct {
				CompressionType string `json:"compression-type"`
			} `json:"data"`
		} `json:"format-specific"`
	}
	require.NoError(t, json.Unmarshal(out, &imgInfo))

	assert.Equal(t, "qcow2", imgInfo.Format)
	assert.EqualValues(t, maxSize, imgInfo.VirtualSize)
	assert.Equal(t, "zstd", imgInfo.FormatSpecific.Data.CompressionType)
}

// Ensures overlay image creation works and yields an overlay image pointing to a base path
func TestCreateOverlayImage(t *testing.T) {
	require.NoError(t, EnsureDependencies())

	tmp := t.TempDir()
	basePath := filepath.Join(tmp, "base.qcow2")
	overlayPath := filepath.Join(tmp, "overlay.qcow2")

	require.NoError(t, CreateBaseImage(basePath, CompressionTypeNone, 10<<20))
	require.NoError(t, CreateOverlayImage(overlayPath, basePath))

	_, err := os.Stat(overlayPath)
	require.NoError(t, err, "overlay image file should exist")

	backing, err := AbsoluteBackingFilename(overlayPath)
	require.NoError(t, err)
	assert.Equal(t, basePath, backing, "overlay should point to the base image path")
}

// Ensures that an overlay image can be mounted and unmounted cleanly
func TestMount(t *testing.T) {
	require.NoError(t, EnsureDependencies())

	imagePath := newTestBaseImage(t)
	mountPoint := filepath.Join(t.TempDir(), "mnt")
	t.Cleanup(func() { _ = Unmount(mountPoint) })

	require.NoError(t, Mount(imagePath, mountPoint))
	assert.True(t, isMountPoint(mountPoint), "expected active mount point after Mount()")

	require.NoError(t, Unmount(mountPoint))
	assert.False(t, isMountPoint(mountPoint), "expected mount point to be inactive after Unmount()")
}

// Ensures that probeRawMountPoint finds the correct raw device mount point when provided
// with a filesystem mount point.
func TestProbeRawMountPoint(t *testing.T) {
	require.NoError(t, EnsureDependencies())

	imagePath := newTestBaseImage(t)
	mountPoint := filepath.Join(t.TempDir(), "mnt")
	t.Cleanup(func() { _ = Unmount(mountPoint) })

	require.NoError(t, Mount(imagePath, mountPoint))

	rawMP, err := probeRawMountPoint(mountPoint)
	require.NoError(t, err)
	assert.True(t, isMountPoint(rawMP), "probed raw mount point should be active")

	require.NoError(t, Unmount(mountPoint))
}

// Ensures that updates to the base image path take effect
func TestRebase(t *testing.T) {
	require.NoError(t, EnsureDependencies())

	tmp := t.TempDir()
	basePath := filepath.Join(tmp, "base.qcow2")
	overlayPath := filepath.Join(tmp, "overlay.qcow2")
	newBasePath := filepath.Join(tmp, "base-moved.qcow2")

	require.NoError(t, CreateBaseImage(basePath, CompressionTypeNone, 10<<20))
	require.NoError(t, CreateOverlayImage(overlayPath, basePath))
	require.NoError(t, os.Rename(basePath, newBasePath))

	require.NoError(t, Rebase(overlayPath, newBasePath))

	backing, err := AbsoluteBackingFilename(overlayPath)
	require.NoError(t, err)
	assert.Equal(t, newBasePath, backing)
}

// Ensures that changes made in an overlay image can be committed into the base image,
// resulting in an empty overlay and an updated base.
func TestCommitToBase(t *testing.T) {
	require.NoError(t, EnsureDependencies())

	basePath := newTestBaseImage(t)
	overlayPath := filepath.Join(t.TempDir(), "overlay.qcow2")
	require.NoError(t, CreateOverlayImage(overlayPath, basePath))

	mountPoint := filepath.Join(t.TempDir(), "mnt")
	t.Cleanup(func() { _ = Unmount(mountPoint) })

	require.NoError(t, Mount(overlayPath, mountPoint))
	require.NoError(t, os.WriteFile(filepath.Join(mountPoint, "probe.txt"), []byte("hello"), 0644))
	require.NoError(t, Unmount(mountPoint))

	require.NoError(t, CommitToBase(overlayPath))

	// The committed file must now be visible by mounting the base directly.
	baseMountPoint := filepath.Join(t.TempDir(), "base-mnt")
	t.Cleanup(func() { _ = Unmount(baseMountPoint) })

	require.NoError(t, Mount(basePath, baseMountPoint))
	data, err := os.ReadFile(filepath.Join(baseMountPoint, "probe.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data), "committed file should be present in base")
	require.NoError(t, Unmount(baseMountPoint))
}

// Ensures CommitToBase is not possible if this (or another) overlay using the same base is mounted
func TestCommitToBaseOverlap(t *testing.T) {
	require.NoError(t, EnsureDependencies())

	basePath := newTestBaseImage(t)

	tmp := t.TempDir()
	overlay1Path := filepath.Join(tmp, "overlay1.qcow2")
	overlay2Path := filepath.Join(tmp, "overlay2.qcow2")

	require.NoError(t, CreateOverlayImage(overlay1Path, basePath))
	require.NoError(t, CreateOverlayImage(overlay2Path, basePath))

	mountPoint1 := filepath.Join(t.TempDir(), "mnt1")
	mountPoint2 := filepath.Join(t.TempDir(), "mnt2")
	t.Cleanup(func() { _ = Unmount(mountPoint1) })
	t.Cleanup(func() { _ = Unmount(mountPoint2) })

	// Case 1: the overlay itself is mounted — its image file is locked.
	require.NoError(t, Mount(overlay1Path, mountPoint1))
	assert.Error(t, CommitToBase(overlay1Path), "CommitToBase should fail when overlay is mounted")
	require.NoError(t, Unmount(mountPoint1))

	// Case 2: a different overlay using the same base is mounted — the base is held open
	// with a read lock by qemu-storage-daemon, so writing to it must fail.
	require.NoError(t, Mount(overlay2Path, mountPoint2))
	assert.Error(t, CommitToBase(overlay1Path), "CommitToBase should fail when another overlay of the same base is mounted")
	require.NoError(t, Unmount(mountPoint2))
}

// Ensures that GrowImage expands the virtual size, resizes the ext4 partition,
// and preserves data written before the resize.
func TestGrowImage(t *testing.T) {
	require.NoError(t, EnsureDependencies())

	const originalSize = 100 << 20 // 100 MiB
	const newSize = 200 << 20      // 200 MiB

	imagePath := filepath.Join(t.TempDir(), "base.qcow2")
	require.NoError(t, CreateBaseImage(imagePath, CompressionTypeNone, originalSize))

	mountPoint := filepath.Join(t.TempDir(), "mnt")
	t.Cleanup(func() { _ = Unmount(mountPoint) })

	require.NoError(t, Mount(imagePath, mountPoint))
	require.NoError(t, os.WriteFile(filepath.Join(mountPoint, "probe.txt"), []byte("hello"), 0644))
	require.NoError(t, Unmount(mountPoint))

	require.NoError(t, Grow(imagePath, newSize))

	out, err := exec.Command(qemuImg, "info", "--output=json", imagePath).Output()
	require.NoError(t, err)
	var imgInfo struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	require.NoError(t, json.Unmarshal(out, &imgInfo))
	assert.EqualValues(t, newSize, imgInfo.VirtualSize)

	require.NoError(t, Mount(imagePath, mountPoint))
	data, err := os.ReadFile(filepath.Join(mountPoint, "probe.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))
	require.NoError(t, Unmount(mountPoint))
}

// Ensures that BaseImagePath returns the correct path to the base image when provided
// with an overlay image.
func TestBaseImagePath(t *testing.T) {
	require.NoError(t, EnsureDependencies())

	tmp := t.TempDir()
	basePath := filepath.Join(tmp, "base.qcow2")
	overlayPath := filepath.Join(tmp, "overlay.qcow2")

	require.NoError(t, CreateBaseImage(basePath, CompressionTypeNone, 10<<20))
	require.NoError(t, CreateOverlayImage(overlayPath, basePath))

	backing, err := AbsoluteBackingFilename(overlayPath)
	require.NoError(t, err)
	assert.Equal(t, basePath, backing)
}
