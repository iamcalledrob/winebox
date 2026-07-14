package winebox

import (
	"context"
	"errors"
	"fmt"
	stdimage "image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"robmason.co.uk/winebox/icons"
)

// DefaultPassthruEnvKeys is a minimal environment suitable for running Wine processes.
// Kept minimal to prevent accidental behaviour changes on system environment
var DefaultPassthruEnvKeys = []string{
	"HOME",
	"USER",
	"PATH",
	"DISPLAY",
	"WAYLAND_DISPLAY",
	"XDG_RUNTIME_DIR",
	"DBUS_SESSION_BUS_ADDRESS",
	"PULSE_SERVER",
	"PIPEWIRE_RUNTIME_DIR",
	"WINEDEBUG",
	"WINEDLLOVERRIDES",
	"WINEFSYNC",
	"WINEESYNC",

	// Apps run without this, but don't inherit GNOME decor and seem to take a different (less optimal)
	// rendering pathway too. e.g. Sketchup window menus render behind the 3D area without XAUTHORITY set.
	"XAUTHORITY",
}

type WinePrefix struct {
	WinePath   string
	PrefixPath string
}

func NewWinePrefix(wineCmd string, prefixPath string) *WinePrefix {
	return &WinePrefix{WinePath: wineCmd, PrefixPath: prefixPath}
}

func (w *WinePrefix) InjectRegistryString(s string) error {
	regFile, err := writeTemp("", "winebox-*.reg", s)
	if err != nil {
		return fmt.Errorf("writing temp file: %w", err)
	}
	defer func() { _ = os.Remove(regFile) }()

	return w.InjectRegistryFile(regFile)
}

func (w *WinePrefix) InjectRegistryFile(regFilePath string) error {
	regFileWinPath, err := ConvertUnixPath(regFilePath)
	if err != nil {
		return fmt.Errorf("converting .reg file path %s: %w", regFilePath, err)
	}

	err = w.Run(context.Background(), "C:\\windows\\regedit.exe", []string{"/S", regFileWinPath}, nil)
	if err != nil {
		return fmt.Errorf("regedit: %w", err)
	}
	return nil
}

// Run starts a windows executable using wine.
// winExePath is a windows path within the wine prefix, e.g. 'C:\windows\notepad.exe'
// extraEnvKeys is a list of environment variable names to pass through to the wine environment.
func (w *WinePrefix) Run(ctx context.Context, winExePath string, wineArgs []string, extraEnvKeys []string) error {
	var err error
	wineArgs, err = replaceUnixPathArgs(wineArgs)
	if err != nil {
		return fmt.Errorf("replacing unix path args: %w", err)
	}

	// Wine should be run from the dir containing the executable (see wine docs)
	// Failing to do this means the executable might not find sibling resources
	var unixExePath string
	unixExePath, err = ConvertWindowsPath(w.PrefixPath, winExePath)
	if err != nil {
		return fmt.Errorf("converting windows path %s: %w", winExePath, err)
	}

	cmd := exec.CommandContext(ctx, w.WinePath, append([]string{winExePath}, wineArgs...)...)

	cmd.Env = BuildEnv(DefaultPassthruEnvKeys)
	cmd.Env = append(cmd.Env, BuildEnv(extraEnvKeys)...)
	cmd.Env = append(cmd.Env, "WINEPREFIX="+w.PrefixPath)

	cmd.Dir = filepath.Dir(unixExePath)

	// Don't capture Stderr/Stdout -- allow it to fall through for debugging purposes.
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout

	err = cmd.Run()
	if err != nil {
		return fmt.Errorf("%s: %w", cmd.String(), err)
	}
	return nil
}

func (w *WinePrefix) Bootstrap(arch string, winver string) error {
	// Use winecfg /v NOT wineboot, in order to create a properly versioned prefix
	// Editing the CurrentVersion regkey can cause issues due to mismatch between prefix on disk and reg.
	// (CurrentVersion says "this is XP", but prefix is not an XP-like prefix)
	winecfgBin := filepath.Join(filepath.Dir(w.WinePath), "winecfg")

	cmd := exec.Command(winecfgBin, "/v", winver)
	cmd.Env = append(BuildEnv(DefaultPassthruEnvKeys), "WINEPREFIX="+w.PrefixPath, "WINEARCH="+arch)
	_, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("winecfg: %w", withStderrOutput(err))
	}

	// Disable winemenubuilder.exe, which auto-creates desktop entries on the host.
	// Not desirable since the wine prefix is not ordinarily mounted, and its location can change.
	err = w.InjectRegistryString("REGEDIT4\r\n\r\n[HKEY_CURRENT_USER\\Software\\Wine\\DllOverrides]\r\n\"winemenubuilder.exe\"=\"\"\r\n")
	if err != nil {
		return fmt.Errorf("injecting registry to disable winemenubuilder: %w", err)
	}

	return nil
}

func (w *WinePrefix) StopWineserver() error {
	wineserverBin := filepath.Join(filepath.Dir(w.WinePath), "wineserver")
	cmd := exec.Command(wineserverBin, "-k")
	cmd.Env = append(BuildEnv(DefaultPassthruEnvKeys), "WINEPREFIX="+w.PrefixPath)
	_, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("stopping wineserver: %w", withStderrOutput(err))
	}
	return nil
}

// ExtractIcon extracts the best (largest) icon from the exe at winExePath.
// winExePath should be in windows format, relative to the wine prefix,
// e.g. 'C:\windows\notepad.exe'
func (w *WinePrefix) ExtractIcon(winExePath string) (pngPath string, err error) {
	// Path argument is in windows format for consistency with 'Run'.
	// This file is a bit inconsistent in this regard, could be improved.
	var unixExePath string
	unixExePath, err = ConvertWindowsPath(w.PrefixPath, winExePath)
	if err != nil {
		err = fmt.Errorf("converting windows path %s: %w", winExePath, err)
		return
	}

	var icon stdimage.Image
	icon, err = icons.ExtractBestIcon(unixExePath)
	if err != nil {
		err = fmt.Errorf("extracting icon from %s: %w", unixExePath, err)
		return
	}

	var f *os.File
	f, err = os.CreateTemp("", filepath.Base(unixExePath)+"-*.png")
	if err != nil {
		err = fmt.Errorf("creating temporary output file: %w", err)
		return
	}
	defer func() { _ = f.Close() }()

	err = png.Encode(f, icon)
	if err != nil {
		err = fmt.Errorf("encoding as png: %w", err)
		return
	}

	pngPath = f.Name()
	return
}

// Session encapsulates setup and teardown of a winebox-managed wine prefix,
// allowing for sharing of mounted images when appropriate.
//
// Could just be a function, but struct allows for clearer arg (field) naming.
type Session struct {
	BackingImagePath string
	ChangesImagePath string

	WineCmd       string
	InjectRegPath string

	ShouldCommit func(lastCommitErr error) bool
}

func (s *Session) Use(do func(prefix *WinePrefix) error) error {
	// No changes path specified, discard changes on exit
	if s.ChangesImagePath == "" {
		name := fmt.Sprintf("changes-%d.qcow2", time.Now().UnixMicro())
		s.ChangesImagePath = filepath.Join(os.TempDir(), name)
		defer func() { _ = os.Remove(s.ChangesImagePath) }()
	}

	// Always use an overlay -- means the backing image is never mounted directly
	// which ensures no opportunity for mutation.
	overlayImage, err := CreateOverlayImage(s.ChangesImagePath, s.BackingImagePath)
	if err != nil {
		return fmt.Errorf("instantiating overlay image: %w", err)
	}

	// SharedImage dedupes calls to mounts/unmount, mounting only on first use,
	// and unmounting only when nobody needs the mount anymore.
	//
	// Allows the same overlay changeset to be used by multiple runs of "winebox",
	// e.g. when running multiple concurrent instances of a windows app that will
	// share a wineserver.
	sharedOverlay := NewSharedImage(overlayImage)

	prefix := NewWinePrefix(s.WineCmd, ImageMountPoint(sharedOverlay))

	// Explicitly stop wineserver, because it keeps itself alive until idle for 3s
	// i.e. it shuts down *after* unmount so will fail to save state:
	// "wineserver: could not save registry branch to userdef.reg : No such file or directory"
	//
	// This *can't* be done by just deferring because multiple runs can share a wineprefix.
	// i.e. this process may not unmount the overlay, and always stopping wineserver would
	// kill any other wine sessions using the same overlay.
	sharedOverlay.OnWillUnmount(func() {
		_ = prefix.StopWineserver()
	})

	err = UseImage(sharedOverlay, func() error {
		// Inject registry into overlay to allow for per-run customization (e.g. DPI)
		if s.InjectRegPath != "" {
			err = prefix.InjectRegistryFile(s.InjectRegPath)
			if err != nil {
				return fmt.Errorf("injecting registry: %w", err)
			}
		}

		return do(prefix)
	})
	if err != nil {
		return fmt.Errorf("using image: %w", err)
	}

	// Allow multiple attempts to commit, as uncommitted changes may be important and
	// there may be temporary, user-fixable errors that prevent commits.
	var commitErr error
tryCommit:
	if s.ShouldCommit == nil || !s.ShouldCommit(commitErr) {
		return nil
	}

	commitErr = overlayImage.Commit()
	if commitErr != nil {
		goto tryCommit
	}

	return nil
}

// ConvertWindowsPath converts a case-insensitive absolute windows path into a unix-style path inside a wine prefix
// e.g. C:\windows\notepad.exe -> {prefixPath}/drive_c/windows/notepad.exe
//
// The prefixPath must be available on disk, as it is used to determine the correct on-disk case.
func ConvertWindowsPath(prefixPath string, windowsPath string) (unixPath string, err error) {
	// Resolve the drive letter symlink (e.g., "c:")
	driveRoot := filepath.Join(prefixPath, "dosdevices", strings.ToLower(windowsPath[:1])+":")

	var root string
	root, err = filepath.EvalSymlinks(driveRoot)
	if err != nil {
		err = fmt.Errorf("evaluating symlinks for drive root '%s': %w", driveRoot, err)
		return
	}

	segments := strings.FieldsFunc(windowsPath[2:], func(r rune) bool { return r == '\\' || r == '/' })
	unixPath = root

	// Build the cased path using case-insensitive patching

outer:
	for _, seg := range segments {
		entries, _ := os.ReadDir(unixPath)

		for _, entry := range entries {
			if strings.EqualFold(entry.Name(), seg) {
				unixPath = filepath.Join(unixPath, entry.Name())
				continue outer
			}
		}

		err = fmt.Errorf("%w: %s", os.ErrNotExist, unixPath)
		return
	}

	return
}

// ConvertUnixPath converts a unix path into a windows-compatible path without the need to (slowly
// boot the wineserver/wineprefix, using lexical parsing only.
//
// We (safely?) assume mapping of z: -> / since the wine prefix is provisioned by winebox
func ConvertUnixPath(unixPath string) (windowsPath string, err error) {
	unixPath, err = filepath.Abs(unixPath)
	if err != nil {
		err = fmt.Errorf("abs: %w", err)
		return
	}

	// If forbidden windows characters (e.g. '|', ':') end up being a problem, we could make a
	// temp safely named symlink and return that.

	windowsPath = "Z:" + strings.ReplaceAll(unixPath, "/", `\`)
	return
}

// Replaces "windows path cast" args to windows-friendly path args.
// Removes empty/blank casts.
//
//	'(win-path)/foo/bar/baz'          -> Z:\foo\bar\baz
//	"(win-path)'/foo/bar/baz'"        -> Z:\foo\bar\baz  (desktop env shell-quoting stripped)
//	'(win-path)'                      -> argument removed
//	'(win-path) '                     -> argument removed
//	'hello'                            -> argument preserved intact
func replaceUnixPathArgs(args []string) (result []string, err error) {
	for _, arg := range args {
		if path := strings.TrimPrefix(arg, "(win-path)"); path != arg {
			// Ignore accidental whitespace
			path = strings.TrimSpace(path)

			// Desktop environments that launch via sh -c shell-quote %f before
			// substituting it. If (win-path)%f is inside desktop-entry "..."
			// the shell can't process those quotes, so they arrive here literally.
			// Strip a single layer of surrounding single or double quotes.
			if len(path) >= 2 && path[0] == '\'' && path[len(path)-1] == '\'' {
				path = path[1 : len(path)-1]
			} else if len(path) >= 2 && path[0] == '"' && path[len(path)-1] == '"' {
				path = path[1 : len(path)-1]
			}

			// Was just "(win-path)", skip arg
			if path == "" {
				continue
			}

			var winPath string
			winPath, err = ConvertUnixPath(path)
			if err != nil {
				err = fmt.Errorf("converting '%s': %w", arg, err)
				return
			}

			result = append(result, winPath)
			continue
		}

		// Not a (win-path) arg.
		result = append(result, arg)
	}

	return
}

func withStderrOutput(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("%w: %s", ee, string(ee.Stderr))
	}

	return err
}
