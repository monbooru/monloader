# Packaging

| Path | Artifact |
|---|---|
| `icons/` | `monloader.ico` for Windows shortcuts and the tray, plus the PNG sizes the Flatpak installs into hicolor |
| `flatpak/` | the manifest, the launcher wrapper, the desktop entry and the AppStream metainfo |
| `windows/` | the Inno Setup script; the workflow stages the installer's own copy of the zip's contents under `windows/payload/` first |
| `release-env.sh` | the version and the ldflags every artifact is stamped with |
| `fetch-tools.sh` | downloads the pinned one-file gallery-dl builds into `tools/` |
| `build-binaries.sh` | assembles every archive into `dist/` |
| `build-installer.sh` | runs ISCC over one staged desktop payload to produce a Windows installer |
| `flatpak-bundle.sh` | installs the runtime and builds the Flatpak bundle; must run unprivileged |
