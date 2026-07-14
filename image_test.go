package winebox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"robmason.co.uk/winebox/qcow2"
)

func newTestBaseImage(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, qcow2.CreateBaseImage(path, qcow2.CompressionTypeNone, 10<<20))
	return path
}

// Ensures that `mountedImages()`:
// 1. Returns mounted winebox images
// 2. Does not return other mounted qcow2 images (i.e. filtering works)
// 3. Does not fail if there are other mounted qcow2 images
func TestMountedImages(t *testing.T) {
	require.NoError(t, qcow2.EnsureDependencies())

	orig := AppRuntimeDir
	AppRuntimeDir = filepath.Join(t.TempDir(), "winebox")
	t.Cleanup(func() { AppRuntimeDir = orig })

	tmp := t.TempDir()

	// Set up a winebox-managed overlay mounted at its expected canonical path.
	wineboxBase := newTestBaseImage(t, tmp, "winebox-base.qcow2")
	wineboxOverlayPath := filepath.Join(tmp, "winebox-overlay.qcow2")
	wineboxOverlay, err := CreateOverlayImage(wineboxOverlayPath, wineboxBase)
	require.NoError(t, err)

	wineboxMount := ImageMountPoint(wineboxOverlay)
	require.NoError(t, os.MkdirAll(wineboxMount, 0700))
	require.NoError(t, qcow2.Mount(wineboxOverlay.Path(), wineboxMount))
	t.Cleanup(func() { _ = qcow2.Unmount(wineboxMount) })

	// Set up a non-winebox overlay mounted at an arbitrary path (not ImageMountPoint).
	otherBase := newTestBaseImage(t, tmp, "other-base.qcow2")
	otherOverlayPath := filepath.Join(tmp, "other-overlay.qcow2")
	otherOverlay, err := CreateOverlayImage(otherOverlayPath, otherBase)
	require.NoError(t, err)

	otherMount := filepath.Join(t.TempDir(), "other-mnt")
	require.NoError(t, os.MkdirAll(otherMount, 0700))
	require.NoError(t, qcow2.Mount(otherOverlay.Path(), otherMount))
	t.Cleanup(func() { _ = qcow2.Unmount(otherMount) })

	images, err := mountedImages()
	require.NoError(t, err, "mountedImages should not fail even when non-winebox images are mounted")

	paths := make([]string, len(images))
	for i, img := range images {
		paths[i] = img.Path()
	}

	assert.Contains(t, paths, wineboxOverlay.Path(), "should include winebox-mounted overlay")
	assert.NotContains(t, paths, otherOverlay.Path(), "should exclude overlay not mounted at its canonical winebox path")
}

func TestCreateOverlayImage(t *testing.T) {
	require.NoError(t, qcow2.EnsureDependencies())

	// Ensures that when no image exists, CreateOverlayImage will create
	// a new qcow2 image pointing at the backing file.
	t.Run("New", func(t *testing.T) {
		tmp := t.TempDir()
		basePath := newTestBaseImage(t, tmp, "base.qcow2")
		overlayPath := filepath.Join(tmp, "overlay.qcow2")

		overlay, err := CreateOverlayImage(overlayPath, basePath)
		require.NoError(t, err)

		_, err = os.Stat(overlayPath)
		require.NoError(t, err, "overlay file should exist on disk")

		backing, err := overlay.BackingImagePath()
		require.NoError(t, err)
		assert.Equal(t, basePath, backing, "overlay should point to the specified backing image")
	})

	// Ensures that when an image already exists, CreateOverlayImage will
	// rebase it to point to the backing file.
	t.Run("Existing", func(t *testing.T) {
		tmp := t.TempDir()
		basePath := newTestBaseImage(t, tmp, "base.qcow2")
		overlayPath := filepath.Join(tmp, "overlay.qcow2")

		_, err := CreateOverlayImage(overlayPath, basePath)
		require.NoError(t, err)

		// Simulate the backing image being moved to a new location.
		newBasePath := filepath.Join(tmp, "base-moved.qcow2")
		require.NoError(t, os.Rename(basePath, newBasePath))

		overlay, err := CreateOverlayImage(overlayPath, newBasePath)
		require.NoError(t, err)

		backing, err := overlay.BackingImagePath()
		require.NoError(t, err)
		assert.Equal(t, newBasePath, backing, "overlay should be rebased to the new backing path")
	})

	// Ensures that when an image already exists and is mounted,
	// CreateOverlayImage will succeed if the mounted image is already
	// backed by the expected backing file, and will fail if it is not
	// (since rebase can't be performed on a mounted image).
	t.Run("ExistingMounted", func(t *testing.T) {
		tmp := t.TempDir()
		basePath := newTestBaseImage(t, tmp, "base.qcow2")
		overlayPath := filepath.Join(tmp, "overlay.qcow2")
		mountPoint := filepath.Join(t.TempDir(), "mnt")

		_, err := CreateOverlayImage(overlayPath, basePath)
		require.NoError(t, err)

		require.NoError(t, qcow2.Mount(overlayPath, mountPoint))
		t.Cleanup(func() { _ = qcow2.Unmount(mountPoint) })

		// Same backing path: rebase is a no-op, so this must succeed.
		_, err = CreateOverlayImage(overlayPath, basePath)
		require.NoError(t, err, "should succeed when overlay is already backed by the specified image")

		// Different backing path: qemu-img rebase needs a write lock, which it can't
		// acquire while the image is mounted, so this must fail.
		otherBasePath := newTestBaseImage(t, tmp, "other-base.qcow2")
		_, err = CreateOverlayImage(overlayPath, otherBasePath)
		assert.Error(t, err, "should fail when a different backing is requested for a mounted overlay")

		require.NoError(t, qcow2.Unmount(mountPoint))
	})

	// Ensures that it's not possible to mount an overlay in a way that
	// means the specified backingImagePath is not used -- e.g. mounting
	// an overlay that is backed by some *other* backing image.
	t.Run("BackingImagePath", func(t *testing.T) {
		tmp := t.TempDir()
		baseA := newTestBaseImage(t, tmp, "base-a.qcow2")
		baseB := newTestBaseImage(t, tmp, "base-b.qcow2")
		overlayPath := filepath.Join(tmp, "overlay.qcow2")

		// Create overlay initially pointing at base A.
		_, err := CreateOverlayImage(overlayPath, baseA)
		require.NoError(t, err)

		// Reconfigure to use base B — must update, not silently remain at A.
		overlay, err := CreateOverlayImage(overlayPath, baseB)
		require.NoError(t, err)

		backing, err := overlay.BackingImagePath()
		require.NoError(t, err)
		assert.Equal(t, baseB, backing, "backing image path must reflect the most recently specified path")
	})
}
