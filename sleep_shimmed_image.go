package winebox

import (
	"errors"
	"fmt"

	"robmason.co.uk/winebox/sleep"
)

// SleepShimmedImage implements a workaround for a Linux kernel bug where a FUSE
// daemon can get paused *before* processes that depend on it. This ordering can
// lead to a sleep hanging and then aborting, e.g. if a process tries disk IO
// inside the FUSE mount before the system goes to sleep.
//
// This shim hooks into the system sleep sequence and manually pauses processes
// that depend on the mount point *before* the system suspends -- i.e. before
// FUSE daemons get paused.
//
// https://ratatoskr.run/fuse-devel/2026/08/17431737/t
type SleepShimmedImage struct {
	raw     Image
	find    sleep.ProcessFinder
	release func()
}

var _ Image = (*SleepShimmedImage)(nil)

func NewSleepShimmedImage(raw Image, find sleep.ProcessFinder) *SleepShimmedImage {
	return &SleepShimmedImage{raw: raw, find: find}
}

func (i *SleepShimmedImage) Mount() error {
	err := i.raw.Mount()
	if err != nil {
		return err
	}

	label := sleep.InhibitLabel{
		Who: "winebox",
		Why: "Gracefully pause wine as workaround for Linux FUSE bug",
	}

	i.release, err = sleep.PauseProcessesDuringSleep(label, i.find)
	if err != nil {
		// Unmount so a failed pause request leaves nothing mounted
		return errors.Join(
			fmt.Errorf("requesting pause during future sleep: %w", err),
			i.raw.Unmount(),
		)
	}

	return nil
}

func (i *SleepShimmedImage) Unmount() error {
	// i.release should be non-nil because calls to Unmount must be balanced
	// with calls to Mount
	i.release()

	return i.raw.Unmount()
}

func (i *SleepShimmedImage) Path() string {
	return i.raw.Path()
}
