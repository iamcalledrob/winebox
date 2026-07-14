package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/c2h5oh/datasize"
	"github.com/manifoldco/promptui"
	"github.com/ncruces/zenity"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"robmason.co.uk/winebox"
	"robmason.co.uk/winebox/qcow2"
)

var version = "dev"

func main() {
	err := run()
	if err != nil {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			_ = zenity.Error(err.Error(),
				zenity.Title("Winebox Error"),
				zenity.ErrorIcon)
		}

		fmt.Printf("ERROR: %s\n", err)
		os.Exit(1)
	}
}

func run() error {
	var root = &cobra.Command{
		Use:     "winebox",
		Short:   "Use wine with read-only prefix images",
		Version: version,
		Long: `
Wrangles a mutable Wine Prefix, i.e. a "bag of state", into a read-only image file.

Changes are captured in a qcow2 overlay image, and can optionally be merged back into the
base image at the end of a terminal-based session, or manually using the "commit" command.

Makes it possible to have a declarative and somewhat reproducible wine experience,
assuming the base image is not modified. The same wine prefix can be used across multiple
computers by syncing or storing the image on a network drive.
		
Depends on a few tools which must be in PATH:
  ` + strings.Join(qcow2.AllDependencies, "\n  ") + `
`,

		// Suppress cobra's built-in "Error: ..." line and usage dump on failure;
		// main() prints the error once via slog.
		SilenceErrors: true,
		SilenceUsage:  true,

		PreRunE: func(cmd *cobra.Command, args []string) error {
			if err := qcow2.EnsureDependencies(); err != nil {
				return fmt.Errorf("ensuring dependencies are available: %w", err)
			}
			return nil
		},
	}

	// Create
	{
		var size string
		var wineCmd string
		var winver string
		var arch string
		cmd := &cobra.Command{
			Use:   "create <image-path> [flags]",
			Short: "Create a new wine prefix image",
			Long: `
Create and bootstrap new wine prefix sparse image.

A sparse image only takes up space on disk when blocks are actually in use. However,
this may not be the case of the image is stored on a network drive.`,
			Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) (err error) {
				imagePath := args[0]

				var sz datasize.ByteSize
				err = sz.UnmarshalText([]byte(size))
				if err != nil {
					return fmt.Errorf("invalid --size %q: %w", size, err)
				}

				fmt.Printf(`
Creating new wine prefix image:
- path: %s
- size: %s
- wine: %s
- winver: %s
- arch: %s
`, imagePath, sz.HumanReadable(), wineCmd, winver, arch)

				var image *winebox.BackingImage
				image, err = winebox.NewBackingImage(imagePath)
				if err != nil {
					return fmt.Errorf("instantiating backing image: %w", err)
				}

				// Actually create file on disk, formatted
				err = image.Materialize(int64(sz))
				if err != nil {
					return fmt.Errorf("materializing backing image: %w", err)
				}

				return winebox.UseImage(image, func() error {
					winePrefixPath := winebox.ImageMountPoint(image)
					p := winebox.NewWinePrefix(wineCmd, winePrefixPath)

					// WineBoot initializes the prefix
					err = p.Bootstrap(arch, winver)
					if err != nil {
						return fmt.Errorf("bootstrapping wine prefix (%s, %s): %w", arch, winver, err)
					}
					return nil
				})
			},
		}

		cmd.Flags().StringVar(&size, "size", "32GB", "max size image can grow up to, e.g. 32GB, 512GB")
		cmd.Flags().StringVar(&wineCmd, "wine", "wine", "wine binary: name in PATH or absolute path")
		cmd.Flags().StringVar(&winver, "winver", "win10", "Windows version to set, e.g. win10, win7")
		cmd.Flags().StringVar(&arch, "arch", "win64", "Windows architecture: win64 or win32")
		root.AddCommand(cmd)
	}

	// Grow
	{
		var size string
		cmd := &cobra.Command{
			Use:   "grow <image-path> --size <new-size>",
			Short: "Grow a wine prefix image to a new size",
			Args:  cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				var sz datasize.ByteSize
				err := sz.UnmarshalText([]byte(size))
				if err != nil {
					return fmt.Errorf("invalid --size %q: %w", size, err)
				}

				var image *winebox.BackingImage
				image, err = winebox.NewBackingImage(args[0])
				if err != nil {
					return fmt.Errorf("instantiating backing image handle: %w", err)
				}

				err = image.Grow(int64(sz))
				if err != nil {
					return fmt.Errorf("growing image: %w", err)
				}

				fmt.Printf("Image grown to %s\n", sz.HumanReadable())
				return nil
			},
		}
		cmd.Flags().StringVar(&size, "size", "", "new size for the image, e.g. 64GB")
		_ = cmd.MarkFlagRequired("size")
		root.AddCommand(cmd)
	}

	// Unmount
	{
		cmd := &cobra.Command{
			Use:   "unmount",
			Short: "Unmount wine prefix images which are 'orphaned': mounted but not in use",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				return winebox.UnmountOrphanedImages()
			},
		}
		root.AddCommand(cmd)
	}

	// Commit
	{
		var changesPath string
		cmd := &cobra.Command{
			Use:   "commit <image-path> --changes <changes-path>",
			Short: "Commit captured changes into a wine prefix image",
			Args:  cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {

				image, err := winebox.CreateOverlayImage(changesPath, args[0])
				if err != nil {
					return fmt.Errorf("instantiating overlay image: %w", err)
				}

				// Commit fails with a useful error (from qemu) if the image is currently mounted.
				err = image.Commit()
				if err != nil {
					return fmt.Errorf("commit: %w", err)
				}

				return nil
			},
		}
		cmd.Flags().StringVar(&changesPath, "changes", "", "overlay file with captured changes")
		_ = cmd.MarkFlagRequired("changes")
		root.AddCommand(cmd)
	}

	// Shell
	{
		var wineCmd string
		var changesPath string
		var regPath string
		var passthruEnv []string
		cmd := &cobra.Command{
			Use:   "shell <image-path> [flags]",
			Short: "Open a shell for manipulating and debugging a wine prefix image",
			Long: `
	Open a shell inside a read-only wine prefix image, which gets temporarily mounted
	in order to do so. The environment is set identically to the 'run' command.
	
	Changes are captured in the --changes image, or are discarded if --changes is not set.
	When the shell exits, you will be prompted whether to commit changes into the image.
				`,
			Args: cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) (err error) {

				s := winebox.Session{
					BackingImagePath: args[0],
					ChangesImagePath: changesPath,
					WineCmd:          wineCmd,
					InjectRegPath:    regPath,
					ShouldCommit: func(lastCommitErr error) bool {
						if lastCommitErr != nil {
							fmt.Printf("Commit failed: %s\n", lastCommitErr.Error())
						}

						// Allow for retry
						return confirm(labelConfirmCommit)
					},
				}

				return s.Use(func(prefix *winebox.WinePrefix) error {
					return enterWinePrefixShell(prefix, passthruEnv)
				})
			},
		}
		cmd.Flags().StringVar(&wineCmd, "wine", "wine", "wine binary: name in PATH or absolute path")
		cmd.Flags().StringVar(&changesPath, "changes", "", "optional overlay file for capturing changes")
		cmd.Flags().StringVar(&regPath, "reg", "", "registry file to inject before entering shell")
		cmd.Flags().StringArrayVar(&passthruEnv, "passthru-env", nil, "include a named env var in the shell environment, e.g. --passthru-env DXVK_HUD (may be repeated)")
		root.AddCommand(cmd)
	}

	// Run
	{
		var changesPath string
		var wineCmd string
		var regPath string
		var passthruEnv []string
		cmd := &cobra.Command{
			Use:   "run <image-path> [flags] -- <executable-path> [args...]",
			Short: "Run a windows executable inside a wine prefix image",
			Long: `
	Run a windows executable inside a read-only wine prefix image, which gets temporarily
	mounted in order to do so.
	
	Changes are captured in the --changes directory, or are discarded if --changes is not set.
	Everything after -- is passed to wine as the command and arguments.
	
	Example: ` + root.Name() + ` run /data/prefix.qcow2 --wine /nix/store/.../wine --changes ~/my-changes.qcow2 -- 'C:\windows\notepad.exe'`,
			Args: cobra.MinimumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) (err error) {

				var wineArgs []string
				if dashIdx := cmd.ArgsLenAtDash(); dashIdx >= 0 {
					wineArgs = args[dashIdx:]
				} else {
					wineArgs = args[1:]
				}

				winExePath := wineArgs[0]
				wineArgs = wineArgs[1:]

				s := winebox.Session{
					BackingImagePath: args[0],
					ChangesImagePath: changesPath,
					WineCmd:          wineCmd,
					InjectRegPath:    regPath,
					ShouldCommit: func(lastCommitErr error) bool {
						if lastCommitErr != nil {
							fmt.Printf("Commit failed: %s\n", lastCommitErr.Error())
						}

						// Allow for retry
						return confirm(labelConfirmCommit)
					},
				}

				// Intercept SIGINT/SIGTERM so that its possible to gracefully shut down
				// (kill child, unmount) rather than just terminating in a mounted state.
				// Consider plumbing this at the cli root if needed across cmds.
				ctx, cancel := winebox.ContextWithCancelSignal(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
				defer cancel()

				return s.Use(func(prefix *winebox.WinePrefix) error {
					err = prefix.Run(ctx, winExePath, wineArgs, passthruEnv)
					if err != nil {
						return fmt.Errorf("running wine: %w", err)
					}
					return nil
				})
			},
		}
		cmd.Flags().StringVar(&wineCmd, "wine", "wine", "wine binary: name in PATH or absolute path")
		cmd.Flags().StringVar(&changesPath, "changes", "", "optional overlay file for capturing changes")
		cmd.Flags().StringVar(&regPath, "reg", "", "registry file to inject before running")
		cmd.Flags().StringArrayVar(&passthruEnv, "passthru-env", nil, "include a named env var in the wine environment, e.g. --passthru-env DXVK_HUD (may be repeated)")
		root.AddCommand(cmd)
	}

	// Icon
	{
		var output string

		cmd := &cobra.Command{
			Use:   "icon <image-path> <executable> [flags]",
			Short: "Extract an icon from a Windows executable in a wine prefix image",
			Long: `Mount a wine prefix image and extract a high resolution png icon from a windows executable
	Example:
	 ` + root.Name() + ` icon /data/prefix.qcow2 'C:\Windows\Notepad.exe'
	 ` + root.Name() + ` icon /data/prefix.qcow2 'C:\Windows\Notepad.exe' --output ~/icons/notepad.png`,
			Args: cobra.ExactArgs(2),
			RunE: func(_ *cobra.Command, args []string) error {
				s := winebox.Session{
					BackingImagePath: args[0],
				}

				return s.Use(func(prefix *winebox.WinePrefix) error {
					extractedIconPath, err := prefix.ExtractIcon(args[1])
					if err != nil {
						return fmt.Errorf("extracting icon from %s: %w", args[1], err)
					}

					// Extracts to a temp location by default, move if user requested
					if output != "" {
						err = os.Rename(extractedIconPath, output)
						if err != nil {
							return fmt.Errorf("moving extracted icon into output location: %w", err)
						}
						extractedIconPath = output
					}

					fmt.Printf("Icon extracted to: %s\n", extractedIconPath)
					return nil
				})
			},
		}

		cmd.Flags().StringVar(&output, "output", "", "output path for icon, e.g. /var/tmp/myapp-icon.png")
		root.AddCommand(cmd)
	}

	return root.Execute()
}

const labelConfirmCommit = "Commit changes into the base image? Changes file will be deleted on success"

func confirm(label string) bool {
	_, err := (&promptui.Prompt{
		Label:     label,
		IsConfirm: true,
	}).Run()
	return err == nil
}
