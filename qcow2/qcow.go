package qcow2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Wrappers for qemu-img ops on qcow2 files, making plenty of assumptions. Not a general-purpose qcow2 pkg.
//
// The gist here is that for a qcow2 image, you first mount it as a raw block device, then mount
// the filesystem *within* that. It's a two-step process because it's implementing a virtual device
// rather than just a filesystem. The exported Mount/Unmount abstract this process by using temporary
// raw mount points for the virtual block devices.

const qemuImg = "qemu-img"
const qemuStorageDaemon = "qemu-storage-daemon"
const mkfsExt4 = "mkfs.ext4"
const fuse2fs = "fuse2fs"
const resize2fs = "resize2fs"
const e2fsck = "e2fsck"

var AllDependencies = []string{qemuImg, qemuStorageDaemon, mkfsExt4, fuse2fs, resize2fs, e2fsck}

func EnsureDependencies() error {
	for _, bin := range AllDependencies {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("'%s' not found in PATH", bin)
		}
	}
	return nil
}

type CompressionType string

const (
	CompressionTypeNone CompressionType = ""
	CompressionTypeZstd                 = "zstd"
)

// CreateBaseImage creates an ext4 formatted qcow2 image file that can grow up to 'maxSize' bytes.
func CreateBaseImage(imagePath string, compressionType CompressionType, maxSize int64) error {
	args := []string{"create", "-f", "qcow2"}
	if compressionType != CompressionTypeNone {
		args = append(args, "-o", "compression_type="+string(compressionType))
	}

	sizeString := fmt.Sprintf("%db", maxSize)
	args = append(args, imagePath, sizeString)

	_, err := exec.Command(qemuImg, args...).Output()
	if err != nil {
		return fmt.Errorf("%s: %w", qemuImg, withStderrOutput(err))
	}

	var rawMountPoint string
	rawMountPoint, err = mkTemp(filepath.Base(imagePath) + "-raw-*")
	if err != nil {
		return fmt.Errorf("making temp raw mount point: %w", err)
	}

	err = mountRaw(imagePath, rawMountPoint)
	if err != nil {
		return fmt.Errorf("mounting raw: %w", err)
	}
	defer func() { _ = unmountRaw(rawMountPoint) }()

	err = formatExt4(rawMountPoint)
	if err != nil {
		return fmt.Errorf("formatting ext4: %w", err)
	}

	return nil
}

// CreateOverlayImage creates a qcow2 image file that overlays a base image to capture changes.
// The 'basePath' can be relative or absolute to 'overlayPath', and gets embedded in the overlay file.
// (i.e. 'basePath' is NOT relative to the working dir)
// Mounting an overlay image inherently mounts the base image too.
func CreateOverlayImage(overlayPath string, backingImagePath string) error {
	_, err := exec.Command(qemuImg, "create", "-f", "qcow2", "-b", backingImagePath, "-F", "qcow2", overlayPath).Output()
	if err != nil {
		return fmt.Errorf("%s: %w", qemuImg, withStderrOutput(err))
	}
	return nil
}

// AbsoluteBackingFilename returns the backing image (or "" if none) for the image at path.
// Derived from full-backing-filename field and resolved to an absolute path.
//
// Dev note: There is some complexity to how backing-filename and full-backing-filename
// work, and what kinds of paths they hold, so always returning an absolute path
// removes any ambiguity for the caller.
func AbsoluteBackingFilename(path string) (string, error) {
	// --force-share allows for reading the metadata without needing a write lock
	o, err := exec.Command(qemuImg, "info", "--force-share", "--output", "json", path).Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", qemuImg, withStderrOutput(err))
	}

	// Factor out into an ImageInfo func if/when more fields are needed
	var info struct {
		FullBackingFilename string `json:"full-backing-filename"`
	}
	err = json.Unmarshal(o, &info)
	if err != nil {
		return "", fmt.Errorf("unmarshaling json: %w", err)
	}

	// No backing filename
	if info.FullBackingFilename == "" {
		return "", nil
	}

	if filepath.IsAbs(info.FullBackingFilename) {
		return filepath.Clean(info.FullBackingFilename), nil
	}

	var abs string
	abs, err = filepath.Abs(info.FullBackingFilename)
	if err != nil {
		return "", fmt.Errorf("abs: %w", err)
	}

	return abs, nil
}

func Mount(imagePath string, mountPoint string) (err error) {
	var rawMountPoint string
	rawMountPoint, err = mkTemp(filepath.Base(imagePath) + "-raw-*")
	if err != nil {
		err = fmt.Errorf("making temp raw mount point: %w", err)
		return
	}

	err = mountRaw(imagePath, rawMountPoint)
	if err != nil {
		err = fmt.Errorf("mounting raw on %s: %w", rawMountPoint, err)
		return
	}

	defer func() {
		if err != nil {
			unmountErr := unmountRaw(rawMountPoint)
			if unmountErr != nil {
				err = errors.Join(err, fmt.Errorf("unmounting raw: %w", unmountErr))
			}
		}
	}()

	err = os.MkdirAll(mountPoint, 0700)
	if err != nil {
		err = fmt.Errorf("ensuring mount point exists: %w", err)
		return
	}

	err = mountFS(rawMountPoint, mountPoint)
	if err != nil {
		err = fmt.Errorf("mounting fs at %s: %w", mountPoint, err)
		return
	}

	return
}

func Unmount(mountPoint string) error {
	rawMountPoint, err := probeRawMountPoint(mountPoint)
	if err != nil {
		return fmt.Errorf("obtaining raw mount point: %w", err)
	}

	err = unmountFS(mountPoint)
	if err != nil {
		return fmt.Errorf("unmounting fs: %w", err)
	}

	err = unmountRaw(rawMountPoint)
	if err != nil {
		return fmt.Errorf("unmounting raw: %w", err)
	}

	return nil
}

// Grow grows the qcow2 image size and the ext4 partition within it to newSize bytes.
func Grow(imagePath string, newSize int64) error {
	_, err := exec.Command(qemuImg, "resize", imagePath, fmt.Sprintf("%db", newSize)).Output()
	if err != nil {
		return fmt.Errorf("%s resize: %w", qemuImg, withStderrOutput(err))
	}

	var rawMountPoint string
	rawMountPoint, err = mkTemp(filepath.Base(imagePath) + "-raw-*")
	if err != nil {
		return fmt.Errorf("making temp raw mount point: %w", err)
	}

	err = mountRaw(imagePath, rawMountPoint)
	if err != nil {
		return fmt.Errorf("mounting raw: %w", err)
	}
	defer func() { _ = unmountRaw(rawMountPoint) }()

	// e2fsck is required by resize2fs
	_, err = exec.Command(e2fsck, "-f", "-p", rawMountPoint).Output()
	if err != nil {
		return fmt.Errorf("e2fsck: %w", withStderrOutput(err))
	}

	_, err = exec.Command(resize2fs, rawMountPoint).Output()
	if err != nil {
		return fmt.Errorf("%s: %w", resize2fs, withStderrOutput(err))
	}

	return nil
}

func CommitToBase(overlayPath string) error {
	_, err := exec.Command(qemuImg, "commit", overlayPath).Output()
	if err != nil {
		return fmt.Errorf("%s commit: %w", qemuImg, withStderrOutput(err))
	}

	err = os.Remove(overlayPath)
	if err != nil {
		return fmt.Errorf("deleting orphaned overlay: %w", err)
	}

	return nil
}

// Rebase updates the backing file path stored in an overlay image.
// Use this when the backing image has moved but its contents are unchanged.
func Rebase(overlayPath string, newBackingFilename string) error {
	_, err := exec.Command(qemuImg, "rebase", "-u", "-b", newBackingFilename, "-F", "qcow2", overlayPath).Output()
	if err != nil {
		return fmt.Errorf("%s rebase: %w", qemuImg, withStderrOutput(err))
	}
	return nil
}

func RebaseIfNeeded(overlayPath string, newBackingFilename string) error {
	oldBackingFilename, err := AbsoluteBackingFilename(overlayPath)
	if err != nil {
		return fmt.Errorf("getting absolute backing filename: %w", err)
	}

	// os.SameFile would be more robust, but is more verbose to get there
	if oldBackingFilename == newBackingFilename {
		return nil
	}

	return Rebase(overlayPath, newBackingFilename)
}

// MountedImages returns paths to all currently mounted qcow2 images on this system.
// Note: This will include any mounts made via this package, but *also* mounts made by
// other means, e.g. by using qemu.
func MountedImages() ([]string, error) {
	rm, err := rawMounts()
	if err != nil {
		return nil, fmt.Errorf("getting raw mounts: %w", err)
	}

	var results []string
	for _, m := range rm {
		results = append(results, m.imagePath)
	}

	return results, nil
}

type rawMount struct {
	imagePath  string
	mountPoint string
}

func rawMounts() ([]rawMount, error) {
	procs, err := findProcessesByName(qemuStorageDaemon)
	if err != nil {
		return nil, fmt.Errorf("finding '%s' procs: %w", qemuStorageDaemon, err)
	}

	var results []rawMount
	for _, p := range procs {
		args, _ := p.CmdLine()

		blockdevArg := nextArg(args, "--blockdev")
		if blockdevArg == "" {
			continue
		}

		exportArg := nextArg(args, "--export")
		if exportArg == "" {
			continue
		}

		imagePath, ok := unmarshalQemuListArg(blockdevArg)["file.filename"]
		if !ok {
			continue
		}

		var rawMountPoint string
		rawMountPoint, ok = unmarshalQemuListArg(exportArg)["mountpoint"]
		if !ok {
			continue
		}

		results = append(results, rawMount{imagePath: imagePath, mountPoint: rawMountPoint})
	}

	return results, nil
}

func unmarshalQemuListArg(s string) map[string]string {
	results := make(map[string]string)
	for _, field := range strings.Split(s, ",") {
		k, v, _ := strings.Cut(field, "=")
		results[k] = v
	}
	return results
}

func nextArg(args []string, needle string) string {
	i := slices.Index(args, needle)
	if i == -1 {
		return ""
	}

	// Needle found, but no next arg
	if i+1 == len(args) {
		return ""
	}

	return args[i+1]
}

func mountRaw(imagePath string, rawMountPoint string) error {
	// Ensure rawMountPoint exists, as qemu-storage-daemon mounts on a file.
	// Equivalent of mkdir -p before using a regular mount point
	err := ensureExists(rawMountPoint, 0644)
	if err != nil {
		return fmt.Errorf("ensuring rawMountPoint exists: %w", err)
	}

	c := exec.Command(qemuStorageDaemon,
		"--blockdev", "driver=qcow2,file.driver=file,file.filename="+qemuEscapeValue(imagePath)+",node-name=node0",
		"--export", "type=fuse,id=exp0,node-name=node0,mountpoint="+qemuEscapeValue(rawMountPoint)+",writable=on",
		"--pidfile", storageDaemonPidFilePath(rawMountPoint),
		"--daemonize",
	)
	_, err = c.Output()
	if err != nil {
		return fmt.Errorf("%s: %w", qemuStorageDaemon, withStderrOutput(err))
	}

	return nil
}

func unmountRaw(rawMountPoint string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid, err := storageDaemonPid(rawMountPoint)
	if err != nil {
		return fmt.Errorf("getting storage daemon pid: %w", err)
	}

	err = killAndWait(ctx, pid)
	if err != nil {
		return fmt.Errorf("terminating %d: %w", pid, err)
	}

	_ = os.Remove(rawMountPoint)
	return nil
}

func formatExt4(rawMountPoint string) error {
	_, err := exec.Command(mkfsExt4, "-E", "root_owner", rawMountPoint).Output()
	if err != nil {
		return fmt.Errorf("%s: %w", mkfsExt4, withStderrOutput(err))
	}
	return nil
}

func mountFS(rawMountPoint string, fsMountPoint string) error {
	// For less cryptic error than "permission denied"
	if isMountPoint(fsMountPoint) {
		return fmt.Errorf("content already mounted here")
	}

	// fuse2fs /tmp/rawblock /mnt/game
	_, err := exec.Command(fuse2fs, rawMountPoint, fsMountPoint).Output()
	if err != nil {
		return fmt.Errorf("%s: %w", fuse2fs, withStderrOutput(err))
	}
	return nil
}

func unmountFS(fsMountPoint string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := fuseUnmountAndWait(ctx, fuse2fs, fsMountPoint)
	if err != nil {
		return fmt.Errorf("terminating %s for %s: %w", fuse2fs, fsMountPoint, withStderrOutput(err))
	}

	return nil
}

func probeRawMountPoint(mountPoint string) (string, error) {
	// fuse2fs is invoked as: fuse2fs <rawDevice> <mountPoint>
	// Walk all fuse2fs processes and find one whose args contain mountPoint,
	// then return the preceding arg which is the raw device path.
	procs, err := findProcessesByName(fuse2fs)
	if err != nil {
		return "", fmt.Errorf("finding %s processes: %w", fuse2fs, err)
	}
	for _, p := range procs {
		args, _ := p.CmdLine()
		for i, arg := range args {
			if arg == mountPoint && i > 0 {
				return args[i-1], nil
			}
		}
	}
	return "", fmt.Errorf("no %s process found for mount point %s", fuse2fs, mountPoint)
}

func mkTemp(pattern string) (string, error) {
	var f *os.File
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return f.Name(), nil
}

func ensureExists(path string, mode fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, mode)
	if err != nil {
		return err
	}
	return f.Close()
}

func storageDaemonPidFilePath(rawMountPoint string) string {
	return rawMountPoint + ".pid"
}

func storageDaemonPid(rawMountPoint string) (int, error) {
	pidFile := storageDaemonPidFilePath(rawMountPoint)
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, fmt.Errorf("reading pidfile: %w", err)
	}

	var pid int
	pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("parsing pidfile contents: %w", err)
	}

	return pid, nil
}

func withStderrOutput(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("%w: %s", ee, string(ee.Stderr))
	}

	return err
}

// qemuEscapeValue escapes a value for use in qemu's comma-separated key=value
// argument format (--blockdev, --export, etc.), where a literal comma is escaped
// as ',,'
func qemuEscapeValue(s string) string {
	return strings.ReplaceAll(s, ",", ",,")
}
