package winebox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

// SharedImage manages the mounting and unmounting of an Image to allow for multiple
// "users" of the image across independent processes.
//
// The underlying Image is mounted when first needed, and only unmounted when no
// user needs the image anymore. This is tracked using a reference counting system.
type SharedImage struct {
	raw Image

	fl *flock.Flock
	rc *PidRefCounter

	onWillUnmount func()
}

func NewSharedImage(raw Image) *SharedImage {
	return &SharedImage{
		raw: raw,
		fl:  flock.New(filepath.Join(AppRuntimeDir, ImageIdentifier(raw.Path()), "lock")),
		rc:  NewPidRefCounter(filepath.Join(AppRuntimeDir, ImageIdentifier(raw.Path()), "refs")),
	}
}

var _ Image = (*SharedImage)(nil)

// Mount only mounts if not already mounted
func (i *SharedImage) Mount() error {
	// Hold file lock during mount op.
	// Prevents race between two processes attempting concurrent mounts,
	// and guards against racey usages of the ref counter
	return useFlock(i.fl, func() error {
		hasReferences, err := i.hasReferences()
		if err != nil {
			return fmt.Errorf("has references: %w", err)
		}

		err = i.rc.Retain()
		if err != nil {
			return fmt.Errorf("retain: %w", err)
		}

		// Existing references: should already be mounted
		if hasReferences {
			return nil
		}

		// No other references, mount.

		// Can be mounted with no references if the last run *crashed*. Be tolerant of this
		// and don't try and double-mount -- which would fail.
		if IsMounted(i) {
			return nil
		}

		return i.raw.Mount()
	})
}

// Unmount only unmounts if noone else holds a reference.
func (i *SharedImage) Unmount() error {
	// Hold file lock during unmount op.
	// Prevents race between two processes attempting concurrent unmounts,
	// and guards against racey usages of the ref counter
	return useFlock(i.fl, func() error {
		err := i.rc.Release()
		if err != nil {
			return fmt.Errorf("release: %w", err)
		}

		return i.unmountIfHasNoReferences()
	})
}

// UnmountIfHasNoReferences unmounts the image if nobody holds a reference.
// Does not decrement the reference counter for this process.
func (i *SharedImage) UnmountIfHasNoReferences() error {
	return useFlock(i.fl, func() error {
		return i.unmountIfHasNoReferences()
	})
}

// OnWillUnmount hook exists to perform pre-unmount cleanup that could prevent
// unmount from succeeding, e.g. killing any wineservers keeping the mount busy.
func (i *SharedImage) OnWillUnmount(do func()) {
	i.onWillUnmount = do
}

func (i *SharedImage) Path() string {
	return i.raw.Path()
}

func (i *SharedImage) hasReferences() (bool, error) {
	n, err := i.rc.ReferenceCount()
	if err != nil {
		return false, fmt.Errorf("getting reference count: %w", err)
	}

	return n > 0, nil
}

func (i *SharedImage) unmountIfHasNoReferences() error {
	has, err := i.hasReferences()
	if err != nil {
		return fmt.Errorf("has references: %w", err)
	}

	if has {
		return nil
	}

	if i.onWillUnmount != nil {
		i.onWillUnmount()
	}

	// Is mounted and has no references, unmount
	return i.raw.Unmount()
}

func useFlock(fl *flock.Flock, use func() error) error {
	err := os.MkdirAll(filepath.Dir(fl.Path()), 0755)
	if err != nil {
		return fmt.Errorf("creating %s: %w", fl.Path(), err)
	}

	err = fl.Lock()
	if err != nil {
		return fmt.Errorf("acquiring file lock: %w", err)
	}

	err = use()

	unlockErr := fl.Unlock()
	if unlockErr != nil {
		err = errors.Join(err, fmt.Errorf("releasing file lock: %w", unlockErr))
	}

	return err
}

func UnmountOrphanedImages() error {
	mounted, err := mountedImages()
	if err != nil {
		return fmt.Errorf("obtaining mounted qcow2 images: %w", err)
	}
	for _, i := range mounted {
		err = NewSharedImage(i).UnmountIfHasNoReferences()
		if err != nil {
			return fmt.Errorf("unmounting %s if no references: %w", i.Path(), err)
		}
	}

	return nil
}
