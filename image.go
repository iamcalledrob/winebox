package winebox

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"robmason.co.uk/winebox/qcow2"
)

type Image interface {
	Mount() error
	Unmount() error
	Path() string
}

type BackingImage struct {
	path string
}

func NewBackingImage(path string) (*BackingImage, error) {
	// Canonical path for ImageMountPoint identifier, which uses path as
	// a deterministic hash input
	canonical, err := canonicalPath(path)
	if err != nil {
		return nil, fmt.Errorf("canonical path: %w", err)
	}
	return &BackingImage{path: canonical}, nil
}

func (b *BackingImage) Materialize(maxSize int64) error {
	return qcow2.CreateBaseImage(b.Path(), qcow2.CompressionTypeZstd, maxSize)
}

func (b *BackingImage) Grow(newSize int64) error {
	return qcow2.Grow(b.path, newSize)
}

func (b *BackingImage) Mount() error {
	return qcow2.Mount(b.Path(), ImageMountPoint(b))
}

func (b *BackingImage) Unmount() error {
	return qcow2.Unmount(ImageMountPoint(b))
}

func (b *BackingImage) Path() string {
	return b.path
}

type OverlayImage struct {
	path string
}

// CreateOverlayImage creates a new overlay image at path, or rebases an existing
// image if it already exists.
func CreateOverlayImage(path string, backingImagePath string) (*OverlayImage, error) {
	// Canonical path for ImageMountPoint identifier, which uses path as
	// a deterministic hash input
	var err error
	path, err = canonicalPath(path)
	if err != nil {
		return nil, fmt.Errorf("canonical path for path: %w", err)
	}

	// Also get a canonical path for the backing image, since it gets embedded
	// verbatim in the overlay qcow2 image. Relative paths can break.
	backingImagePath, err = canonicalPath(backingImagePath)
	if err != nil {
		return nil, fmt.Errorf("canonical path for backingImagePath: %w", err)
	}

	// Materialize a new overlay if needed.
	if _, err = os.Stat(path); errors.Is(err, os.ErrNotExist) {
		err = qcow2.CreateOverlayImage(path, backingImagePath)
		if err != nil {
			return nil, fmt.Errorf("creating overlay image: %w", err)
		}

		return &OverlayImage{path: path}, nil
	}

	// Existing overlay - rebase to allow for backing image to be moved around on disk
	//
	// RebaseIfNeeded only rebases if the backingImagePath has changed, which has the
	// side effect of preventing the "failed to get write lock" error that would arise
	// on every call when the image is already mounted.
	err = qcow2.RebaseIfNeeded(path, backingImagePath)
	if err != nil {
		return nil, fmt.Errorf("updating backing image path: %w", err)
	}
	return &OverlayImage{path: path}, nil
}

func (o *OverlayImage) Mount() error {
	return qcow2.Mount(o.Path(), ImageMountPoint(o))
}

func (o *OverlayImage) Unmount() error {
	return qcow2.Unmount(ImageMountPoint(o))
}

func (o *OverlayImage) Commit() error {
	return qcow2.CommitToBase(o.Path())
}

func (o *OverlayImage) Path() string {
	return o.path
}

func (o *OverlayImage) BackingImagePath() (string, error) {
	return qcow2.AbsoluteBackingFilename(o.path)
}

// ImageMountPoint is the deterministic path that this image will always be mounted at (by us, anyway).
// The path is based on its file path -- so all users of the image can know with reasonable certainty
// whether the image is already mounted
// e.g. /run/user/1000/winebox/example.qcow2-5d41402abc4b/mnt
func ImageMountPoint(image Image) string {
	return filepath.Join(AppRuntimeDir, ImageIdentifier(image.Path()), "mnt")
}

func ImageIdentifier(path string) string {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(path)))[:12]
	// Includes base name for ease of human recognition
	return fmt.Sprintf("%s-%s", filepath.Base(path), hash)
}

func IsMounted(image Image) bool {
	return isMountPoint(ImageMountPoint(image))
}

// mountedImages returns all winebox-mounted qcow2 images by inspecting the system
// Only returns images mounted at their expected canonical location, which helps to exclude
// mounts that did not orignate from winebox.
func mountedImages() ([]Image, error) {
	imagePaths, err := qcow2.MountedImages()
	if err != nil {
		return nil, fmt.Errorf("finding mounted images: %w", err)
	}

	var results []Image
	for _, path := range imagePaths {
		var backingImagePath string
		backingImagePath, err = qcow2.AbsoluteBackingFilename(path)
		if err != nil {
			return nil, fmt.Errorf("%s: getting backing filename: %w", path, err)
		}

		var image *OverlayImage
		image, err = CreateOverlayImage(path, backingImagePath)
		if err != nil {
			return nil, fmt.Errorf("%s: instantiating overlay image: %w", path, err)
		}

		// Either an overlay was caught *just* before unmount, or this image is not mounted
		// where winebox expects it to be -- i.e. it was not mounted by winebox.
		if !IsMounted(image) {
			continue
		}
		results = append(results, image)
	}

	return results, nil
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

// UseImage surrounds the `use` func with Mount/Unmount calls on the image,
// and joins use and unmount errors (i.e. rather than _ = i.Unmount) to ensure
// unexpected issues get picked up. It's important to know if an unmount fails,
// as this would be indicative of a bug.
func UseImage(i Image, use func() error) error {
	err := i.Mount()
	if err != nil {
		return fmt.Errorf("mount: %w", err)
	}

	err = use()

	unmountErr := i.Unmount()
	if unmountErr != nil {
		err = errors.Join(err, fmt.Errorf("unmount: %w", unmountErr))
	}

	return err
}
