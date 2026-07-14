# Winebox

Wine prefixes are fiddly to set up right, and accumulate state over time. It's hard to remember how
to reproduce a wine prefix on a second computer. You're starting from scratch every time.

**Winebox lets you get your programs working in Wine *once*, then use them forever -- on all your machines!**

## How it works
Winebox boxes your Wine prefix into a portable image file. Prefix images are read-only: runtime
changes are captured in a separate overlay. Changes can be discarded, or you can explicitly
commit them back into the base prefix image.

Winebox is designed knowing that a lot of Windows programs have to be set up graphically. It doesn't
try and fight that. You set an image up once, manually (perhaps clicking installer buttons!), then ideally
never need to touch the image again.

A wine prefix can be shared across multiple computers by syncing the image file.
This workflow makes it possible to have a declarative and reproducible Wine experience.

Works with any Linux distribution, and has a Nix flake to declaratively deploy Windows apps on NixOS,
including automatic icon extraction and desktop integration.


## Installation
### NixOS
Add the Nix Flake to your inputs and import the module:
```nix
# In your system flake.nix
inputs.winebox.url = "git+https://robmason.co.uk/winebox";

# Pass inputs through to your NixOS configs, then import the module:
imports = [ inputs.winebox.nixosModules.default ];
```

Optionally, add `winebox` to your `environment.systemPackages`. Or just pull it in
when needed via `nix shell`.

### Other OSs
Install via `go install`, which will install into `GOBIN`. You'll need go 1.26+.
```
$ go install robmason.co.uk/winebox/cmd/winebox@latest
```

## Usage

Here's an example where we set up SketchUp Make 2017.

### 1. Create and set up an image
```bash
# Create the image file
$ winebox create ~/storage/sketchup.qcow2

# Use `winebox shell` to install software.
# You get dropped into a shell at the mounted wine prefix root
$ winebox shell ~/storage/sketchup.qcow2
[drive_c]$

# Use tools like winetricks to set up dependencies
[drive_c]$ winetricks ie8 vcrun2015
[drive_c]$ wine ~/Downloads/sketchupmake-installer.exe

# Ensure it runs as expected
[drive_c]$ wine 'Program Files/SketchUp/SketchUp 2017/SketchUp.exe'

# Exit the winebox shell.
[drive_c]$ exit

# Choose 'y' at the prompt
Commit changes into the base image? Changes file will be deleted on success [y/N]: y
```

On NixOS, you could launch into a shell with winebox, wine, winetricks etc. available
by running something like:
```bash
$ nix shell git+https://robmason.co.uk/winebox nixpkgs#wine nixpkgs#winetricks
```


### 2. Use the image (non-NixOS)
You can use run programs that are inside an image file by using `winebox run`, for example
```bash
$ winebox run ~/sketchup.qcow2 -- 'C:\Program Files\SketchUp\SketchUp 2017\SketchUp.exe'
```

Use the `--changes` flag to save runtime changes into a file of your choosing.

You can have multiple different changesets, and each one gets its own wineserver. This is
particularly useful for programs like SketchUp that have trouble running multiple instances
on a single wineserver.
```bash
# Run, saving changes to a folder.
# Multiple instances will share a wineserver if the same changes path is used. 
$ winebox run ~/sketchup.qcow2 --changes ~/.local/sketchup-changes.qcow2 -- 'C:\Program Files\SketchUp\SketchUp 2017\SketchUp.exe'
$ winebox run ~/sketchup.qcow2 --changes ~/.local/sketchup-changes.qcow2 -- 'C:\Program Files\SketchUp\SketchUp 2017\SketchUp.exe'

# Specifying different change paths will use separate wineservers for each instance. 
$ winebox run ~/sketchup.qcow2 --changes ~/.local/changes-FOO.qcow2 -- 'C:\Program Files\SketchUp\SketchUp 2017\SketchUp.exe'
$ winebox run ~/sketchup.qcow2 --changes ~/.local/changes-BAR.qcow2 -- 'C:\Program Files\SketchUp\SketchUp 2017\SketchUp.exe'
```

The `winebox run` and `winebox shell` commands use the same environment, so if it works
in `winebox shell`, it should work when run with the `run` verb too!


### 3. Use the image (NixOS)
Use the Nix Flake to provision Windows apps declaratively on all your machines.
You'll need somewhere shared to store image files, which all machines can access.

Import `inputs.winebox.nixosModules.default` once in your system config, then declare
apps using `winebox.apps.<name>`. The module automatically installs the launcher,
desktop entry, and icon extraction services for each app.

```nix
# In your system flake.nix, add winebox to inputs and pass it through:
# inputs.winebox.nixosModules.default

{ inputs, pkgs, ... }:
{
  imports = [ inputs.winebox.nixosModules.default ];

  # Declare the app
  winebox.apps.sketchup = {
    image       = "~/shared-storage/sketchup.qcow2";
    cmd         = ''C:\Program Files\SketchUp\SketchUp 2017\SketchUp.exe'';
    wine        = "${pkgs.wine}/bin/wine";
    desktopName = "SketchUp Make 2017";
    desktopExecArgs = ''"(win-path)%f"'';
    categories  = [ "Graphics" "3DGraphics" ];
  };
}
```

NixOS makes it easy to have many different versions of wine installed at the same time, so
each app can specify the wine runner that works best for it. The
[nix-gaming](https://github.com/fufexan/nix-gaming/tree/master/pkgs/wine) project has a few
wine builds which are better tuned for gaming and multimedia. You probably want to pin a
specific known-good version of wine to use with each winebox app. 

#### winebox.apps.\<name\> options:
```nix
winebox.apps.<name> = {
  image           =   # path to .qcow2 file; ~ is expanded at runtime (required)
  cmd             =   # Windows path to executable, e.g. 'C:\windows\notepad.exe' (required)
  wine            =   # path to wine binary, e.g. "${pkgs.wine}/bin/wine" (required)
  changes         =   # path for image to persist changes; ~ is expanded at runtime (optional)
  reg             =   # .reg file to inject before launch (optional)
  passthruEnv     =   # env var names to pass through to wine, e.g. [ "DXVK_HUD" ] (optional)
  desktopName     =   # display name in the app launcher; defaults to <name>
  desktopExecArgs =   # extra args appended to the desktop entry Exec line, e.g. "(win-path)%f" for opening files
  mimeTypes       =   # desktop entry MIME types, e.g. [ "application/x-extension-skp" ]
  categories      =   # desktop entry categories; defaults to [ "Application" ]
  desktop         =   # set to false to suppress desktop entry generation; defaults to true
};
```

The `(win-path)` string in `desktopExecArgs` acts as a cast from a unix-style path into a
windows-style path. Useful for when the program is associated with a mime type, and is
launched with a file. The system provides a unix-style path via `%f`.

For each app the module installs:
- **A wrapper script** in `PATH` that launches the app via `winebox run` (subcommands: `shell`, `extract-icon`)
- **A desktop entry** (unless `desktop = false`) with an automatically-extracted icon

#### Icon extraction details
Winebox can extract icon resources from executables inside images with the `winebox icon`
command. *Choosing the right time* to extract is slightly more complicated. Winebox's nix
module does not require the prefix image to be available during a system rebuild, so the icon
can't be extracted at build-time. The icon can also change, e.g. as app updates are installed.

Winebox's approach for extracting icons for the desktop entry is:
1. Try and extract icons during a system rebuild, if the .qcow2 happens to be available
2. Otherwise, extract the icon (if needed) on launch, before calling `winebox run`
3. Run `appname extract-icon` to manually trigger extraction.

The wine-native `winemenubuilder` is suppressed, as it is not well suited to the temporary
paths used by winebox wine prefixes.

## Requirements
If using Nix Flake, dependencies are handled automatically.

On other distros, the following must be in `PATH`:
- `e2fsck`
- `fuse2fs`
- `mkfs.ext4`
- `qemu-img`
- `qemu-storage-daemon`
- `resize2fs`

You must also either have `wine` in your PATH, or use the `--wine` flag.

### qemu?
Winebox requires qemu for its excellent qcow2 image format. Winebox doesn't use qemu itself,
and does not require qemu to be running nor any kernel modules to be loaded. By using the qcow2
format, wine prefix images only take up the space of the data they contain, and are compressed
on-the-fly automatically (winebox uses zstd).

## Limitations

### Multiple executables with the NixOS module
The NixOS module design assumes a single key executable in each image. It's not well suited
to a "suite" of applications (yet). You would have to define multiple `winebox.apps.<name>`
for each application in the suite, targetting the same .qcow2. If the suite needs to share
state, you'll need to use a common `changes` path.
Otherwise, each executable will run in isolation.  

### Orphaned changes
Committing changes to the base prefix image will lead to a corrupt runtime state if you attempt
to use that base image with a *different* set of overlaid changes. The qcow2 image layering
doesn't defend against this. This is a known problem -- the solution is to not do that.

### Machine-specific state
When sharing a winebox image between machines, you might want to layer in machine-specific
state. For example, windows display DPI or a game's graphics config. Provide a `--changes`
image so that these settings persist between launches. A limitation is that these changes
will become orphaned if the base image is updated. To remedy this situation, you'll need to
clear your local changes and re-apply the machine-specific state.

As a band-aid, a `--reg` option can be passed to `winebox` which will inject a .reg file
at runtime before each launch.

Support could be added for a folder tree to be injected at runtime in the future, e.g. to
inject config files. This is more complex than .reg injection, which is straightforward
because the registry can be merged. Perhaps a diff/patch format could be considered. 

### Manual image setup
Winebox is currently designed with the assumption that you will manually provision your
wine prefix image. Installing winetricks and software "by hand". This is largely because
other efforts in this space focus on automating this, and it tends to be challenging
because so much Windows software is set up via a GUI.

This being said, you could script `winebox create` + `winebox run` + `winebox commit` to
automate provisioning of images. Room for improvement to make this better.

### Couldn't I just share my wine prefix folder by putting it in Dropbox?
Well yes, but there a number of issues with this approach: (1) A partial sync would lead
to undefined behaviour, (2) A wine prefix can contain an extremely high number of files,
which won't sync well, and (3) Wine prefix state will drift over time. A change on one
machine might break another. Also (4), a wine prefix really needs to be stored on a
filesystem that supports symlinks to work properly, which cloud storage providers like
Dropbox don't support. Winebox qcow2 images use a real ext4 filesystem.

## Development notes
- Winebox is really just an orchestrator around a bunch of fs tools and qemu-img.
- Written in Go because we can handle errors rigorously. And because I wanted to.
- You could implement 80% of winebox there with a few bash scripts, but the remaining 20%
  that makes the experience stable is overcomplex for bash.
- This is not a vibe coded project. 
  Core code is mostly human written because AI makes too many mistakes. In particular,
  LLMs don't seem to grasp edge cases around FUSE and introduce unnecessary complexity.
- Claudio scaffolded the nix module, the `icons` package, and tests.
- The root dir (`package winebox`) contains the core functionality.
  The `cmd/winebox` dir glues it all together.

## TODOs:
- [ ] Consider an image creation helper to create and provision from a script
- [ ] Better defense against orphaned overlays. Some exploration was done where metadata
      could be embedded in the .qcow2 files