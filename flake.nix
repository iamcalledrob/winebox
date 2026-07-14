{
  description = "Wine prefix manager using image files";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems       = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (pkgs: {
        default =
          let v = pkgs.lib.trim (builtins.readFile ./VERSION); in
          pkgs.buildGoModule {
          pname = "winebox";
          version = v;
          src = ./.;

          ldflags = [ "-X main.version=${v}" ];

          subPackages = [ "cmd/winebox" ];

          vendorHash = "sha256-3z8VbrHTi3eSLHiAASellrer/6vs4vMkYIYR12ianm4=";

          nativeBuildInputs = [ pkgs.makeWrapper ];

          postInstall = ''
            wrapProgram $out/bin/winebox \
              --prefix PATH : ${pkgs.lib.makeBinPath [
                pkgs.e2fsprogs      # mkfs.ext4, resize2fs, e2fsck
                pkgs.fuse2fs
                pkgs.qemu
                pkgs.zenity         # for gui error dialogs

                # fuse3/fusermount3 intentionally omitted: NixOS provides a setuid-wrapped
                # fusermount3 at /run/wrappers/bin/fusermount3 via security.wrappers.
                # Adding the nix store fuse3 here would shadow it with a non-setuid binary,
                # causing fusermount3 -u to fail and leaving mounts orphaned.
              ]}
          '';

          meta = with pkgs.lib; {
            description = "Manage Wine prefixes using sparse ext4 images and overlayfs";
            license = licenses.gpl3;
            platforms = systems;
          };
        };
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go
            gopls
            gotools
            delve
            e2fsprogs      # mkfs.ext4
            fuse2fs
            fuse3          # fusermount3
            qemu
            zenity
          ];
        };
      });

      nixosModules.default = { config, pkgs, lib, ... }:
        let
          cfg     = config.winebox.apps;
          winebox = self.packages.${pkgs.system}.default;

          # Build per-app derivations up front so both systemPackages and
          # activationScripts can reference the same extractIconScript store path.
          appDrvs = lib.mapAttrs (appName: app: rec {

            # Extract the executable's icon if there is no existing icon, or the
            # image file is newer than the existing icon. Best effort.
            extractIconScript = pkgs.writeShellScript "winebox-${appName}-extract-icon" ''
              _image=${lib.escapeShellArg app.image}
              _image="''${_image/#~/$HOME}"

              [[ -f "$_image" ]] || exit 0

              _icon_dir="$HOME/.local/share/icons/hicolor/256x256/apps"
              _icon="$_icon_dir/winebox-${appName}.png"
              mkdir -p "$_icon_dir"

              if [[ -f "$_icon" ]] && ! [[ "$_image" -nt "$_icon" ]]; then
                exit 0
              fi

              ${winebox}/bin/winebox icon "$_image" ${lib.escapeShellArg app.cmd} --output "$_icon"

              # --force required to actually do the update!
              ${pkgs.gtk3}/bin/gtk-update-icon-cache --force \
                "$HOME/.local/share/icons/hicolor" 2>/dev/null || true
            '';

            # Launch the app, extracting icons up front (if needed).
            # Supports subcommands: `appname shell` and `appname extract-icon`
            launcher = pkgs.writeShellScriptBin appName ''
              if [[ "''${1:-}" == "extract-icon" ]]; then
                exec ${extractIconScript}
              fi
              ${extractIconScript} 2>/dev/null || true
              _image=${lib.escapeShellArg app.image}
              _image="''${_image/#~/$HOME}"
              _args=("$_image" "--wine" ${lib.escapeShellArg app.wine})
              ${lib.optionalString (app.changes != null) ''
                _changes=${lib.escapeShellArg app.changes}
                _changes="''${_changes/#~/$HOME}"
                _args+=("--changes" "$_changes")
              ''}
              ${lib.optionalString (app.reg != null) ''
                _args+=("--reg" ${lib.escapeShellArg "${app.reg}"})
              ''}
              ${lib.concatMapStrings (e: ''
                _args+=("--passthru-env" ${lib.escapeShellArg e})
              '') app.passthruEnv}
              if [[ "''${1:-}" == "shell" ]]; then
                exec ${winebox}/bin/winebox shell "''${_args[@]}"
              else
                exec ${winebox}/bin/winebox run "''${_args[@]}" -- ${lib.escapeShellArg app.cmd} "$@"
              fi
            '';

            desktopItems = lib.optional app.desktop (pkgs.makeDesktopItem {
              name        = appName;
              desktopName = app.desktopName;
              exec        = lib.concatStringsSep " " (
                              [ "${launcher}/bin/${appName}" ] ++
                              lib.optional (app.desktopExecArgs != "") app.desktopExecArgs
                            );
              icon        = "winebox-${appName}";
              mimeTypes   = app.mimeTypes;
              categories  = app.categories;
            });
          }) cfg;
        in
        {
          options.winebox.apps = lib.mkOption {
            default     = {};
            description = "Winebox Windows app definitions.";
            type = lib.types.attrsOf (lib.types.submodule ({ name, ... }: {
              options = {
                image = lib.mkOption {
                  type        = lib.types.str;
                  description = "Path to the .img file. A leading ~ is expanded at runtime.";
                };
                cmd = lib.mkOption {
                  type        = lib.types.str;
                  description = "Windows path to the executable. Use Nix '' strings to avoid backslash escaping, e.g. ''C:\\Program Files\\App\\App.exe''.";
                };
                wine = lib.mkOption {
                  type        = lib.types.str;
                  description = "Path to the wine binary, e.g. \"${pkgs.wine}/bin/wine\".";
                };
                changes = lib.mkOption {
                  type        = lib.types.nullOr lib.types.str;
                  default     = null;
                  description = "Optional path for persisting overlay changes. A leading ~ is expanded at runtime.";
                };
                reg = lib.mkOption {
                  type        = lib.types.nullOr lib.types.path;
                  default     = null;
                  description = "Optional .reg file to inject before launch.";
                };
                desktopName = lib.mkOption {
                  type        = lib.types.str;
                  default     = name;
                  description = "Display name shown in the application launcher. Defaults to the app name.";
                };
                desktopExecArgs = lib.mkOption {
                  type        = lib.types.str;
                  default     = "";
                  description = "Additional args to pass to cmd when launched via desktop entry, e.g. '(win-path)%f'";
                };
                mimeTypes = lib.mkOption {
                  type        = lib.types.listOf lib.types.str;
                  default     = [];
                  description = "Desktop entry mime types";
                };
                categories = lib.mkOption {
                  type        = lib.types.listOf lib.types.str;
                  default     = [ "Application" ];
                  description = "Desktop entry categories.";
                };
                passthruEnv = lib.mkOption {
                  type        = lib.types.listOf lib.types.str;
                  default     = [];
                  description = "Environment variable names to pass through to wine from the calling environment, e.g. [ \"DXVK_HUD\" \"WINEDEBUG\" ].";
                };
                desktop = lib.mkOption {
                  type        = lib.types.bool;
                  default     = true;
                  description = "Whether to generate a desktop entry. Set to false to suppress.";
                };
              };
            }));
          };

          config = {
            environment.systemPackages = lib.flatten (lib.mapAttrsToList (_: drv:
              [ drv.launcher ] ++ drv.desktopItems
            ) appDrvs);

            xdg.mime.defaultApplications = lib.mkMerge (lib.mapAttrsToList (appName: app:
              lib.genAttrs app.mimeTypes (_: [ "${appName}.desktop" ])
            ) cfg);

            environment.extraSetup = ''
              if [ -d "$out/share/mime/packages" ]; then
                ${pkgs.shared-mime-info}/bin/update-mime-database "$out/share/mime"
              fi
            '';

            # Try to extract icons at rebuild time for currently logged-in users.
            # Falls back to launcher-time extraction (above) for fresh boots / new logins.
            system.activationScripts = lib.mapAttrs' (appName: drv:
              lib.nameValuePair "winebox-icon-${appName}" ''
                for _xdg in /run/user/*/; do
                  [[ -d "$_xdg" ]] || continue
                  _uid=$(basename "$_xdg")
                  _user=$(id -nu "$_uid" 2>/dev/null) || continue
                  _home=$(getent passwd "$_user" | cut -d: -f6) || continue
                  HOME="$_home" ${pkgs.util-linux}/bin/runuser -u "$_user" -- \
                    ${drv.extractIconScript} 2>/dev/null || true
                done
              ''
            ) appDrvs;
          };
        };
    };
}
