# Packaging

| Path | Artifact |
|---|---|
| `icons/` | `monloader.ico` for Windows shortcuts and the tray, plus the PNG sizes the Flatpak installs into hicolor |
| `flatpak/` | the manifest, the launcher wrapper, the desktop entry and the AppStream metainfo |
| `appimage/` | the AppRun the AppImage runtime executes |
| `monloader.desktop` | the desktop entry the AppImage carries |
| `aur/` | the PKGBUILD the AUR package is stamped from, and the licence for those packaging sources |
| `windows/` | the Inno Setup script; the workflow stages the installer's own copy of the archive's contents under `windows/payload/` first |
| `release-env.sh` | the version and the ldflags every artifact is stamped with |
| `install-go.sh` | installs the pinned Go toolchain from each workflow's `GO_VERSION` / `GO_SHA256_LINUX_AMD64` and puts it on the job's PATH |
| `fetch-tools.sh` | downloads the pinned one-file gallery-dl builds into `tools/` |
| `build-binaries.sh` | assembles every portable archive into `dist/`. |
| `build-installer.sh` | runs ISCC over one staged payload - the archive's contents minus that marker, so an install never goes portable - to produce a Windows installer |
| `flatpak-bundle.sh` | installs the runtime and builds the Flatpak bundle; must run unprivileged |
| `build-appimage.sh` | builds the bundled AppImage, from the same `tools/` |
| `build-aur.sh` | stamps the PKGBUILD with the version and the tag tarball's checksum, and generates its `.SRCINFO` |
