# Building the native Windows installer

`Product.wxs` is the WiX source for `ISP-BSS-Setup-<version>.msi` — the
single-file install for [scripts/windows/](../scripts/windows/README.md)'s
native Windows deployment. `build.ps1` is the one command that turns it,
the two Go services, and a PostgreSQL binaries zip into that MSI.

## Prerequisites

- **Go** (1.23+ — see the root `go.mod`). Anything that can
  `GOOS=windows GOARCH=amd64 go build` this module.
- **WiX Toolset v5.x**, on `PATH` as `wix` or pointed at with `-WixExe`:
  ```powershell
  dotnet tool install --global --version 5.0.2 wix
  ```
  **Pin to 5.x, not 6 or 7.** FireGiant, which now stewards WiX, introduced
  an Open Source Maintenance Fee EULA starting with v6
  (<https://wixtoolset.org/osmf/>) — running `wix build` on those versions
  refuses until that EULA is accepted with `--acceptEula`. Whether to agree
  to it (and any obligations that come with it for this project) is not
  something `build.ps1` decides on its own; it deliberately targets the
  last pre-OSMF release instead. `wix --version` should print `5.x`.
- **A PostgreSQL 15 Windows binaries zip, already extracted.** Get the
  "binaries" zip (not the interactive installer) from
  <https://www.postgresql.org/download/windows/> and extract it somewhere;
  its root — the directory directly containing `bin\`, `lib\`, `share\` —
  is what `-PgsqlSource` points at. Not fetched automatically and not
  vendored into this repo, the same way
  [setup_postgres.ps1](../scripts/windows/setup_postgres.ps1) has never
  bundled PostgreSQL itself, only the scripts that provision it.

## Build

```powershell
.\build.ps1 -PgsqlSource 'D:\pgsql' -Version 1.0.0
```

Produces `dist\ISP-BSS-Setup-1.0.0.msi`. `-Version` has to be a bare
`major.minor.build` (MSI's own package-version rule — no `1.0.0-rc1`-style
suffixes; `build.ps1` rejects one before ever invoking `wix build` over it).

`stage\` and `obj\` are working directories `build.ps1` recreates on every
run (cross-compiled binaries, the copied-in PostgreSQL tree, and WiX's own
cabinet-building intermediates); none of the three output directories are
committed — see `.gitignore`.

### A drive-space note from actually building this

`wix build` stages every harvested file — the entire bundled PostgreSQL
tree included, several hundred MB — into an intermediate folder before
cabinet-ing it, and defaults that folder to somewhere under `%TMP%`, **independent of where `-StageDir` or `-o` point**. On a machine where `%TMP%`
resolves to a drive that is short on room, this fails opaquely
(`ERROR_DISK_FULL` while creating a CAB, easy to misread as an out-of-disk
problem with the *output* rather than a temp directory nowhere near it).
`build.ps1` passes `-intermediatefolder` explicitly, next to `stage\`, so
the whole build stays on one drive — the one actually holding this
repository — rather than depending on wherever the system temp happens to
be.

## What the MSI does

Lays down the two Go binaries, the bundled PostgreSQL, and the
`scripts/windows/` PowerShell under `Program Files\ISP BSS`, creates an
empty `ProgramData\ISP BSS\config`, then runs
[install.ps1](../scripts/windows/install.ps1) once as a single deferred,
elevated custom action — generate the PostgreSQL superuser password,
`setup_postgres.ps1`, `bootstrap.exe`, `register_services.ps1`, in that
order. Uninstall runs [uninstall.ps1](../scripts/windows/uninstall.ps1) the
same way, which stops and removes both registered services (and the
bundled PostgreSQL's own service) but never touches `pgdata\` or
`config\` — see that script's own reasoning for why data always outlives
the software that used it.

Both custom actions run the actual PowerShell scripts directly rather than
modelling any of this in WiX's own `ServiceInstall` element or in multiple
custom actions passing state between them — see `Product.wxs`'s own
comments for why: the services need registry `Environment` values,
Event Log sources and recovery actions `ServiceInstall` does not expose,
and MSI deferred custom actions cannot see ordinary session properties,
which is exactly the kind of plumbing a single already-tested script avoids
needing at all.

## Installing or upgrading

```
deploy.bat
```

Elevates itself (a UAC prompt, then a new Administrator window), stops the
three services if they're running, runs `msiexec /i` against
`dist\ISP-BSS-Setup-1.0.0.msi`, and reports service status plus a
`/readyz` check afterward. Logs to `dist\install.log`.

This is a single-transaction **upgrade**, not uninstall-then-install, even
over an existing install — `Product.wxs`'s `MajorUpgrade` element and fixed
`UpgradeCode` already make one `/i` call remove the old version and lay
down the new one atomically. A separate manual uninstall first is exactly
what produced a stuck Windows Installer transaction during this project's
own installer debugging (locked PostgreSQL files renamed to `.tmp`,
queued for deletion on next reboot, leaving every following `msiexec` call
failing with "Another program is being installed" until a reboot cleared
it) — `deploy.bat` stops the services itself before `msiexec` ever runs
specifically to avoid re-creating that.

A real uninstall-then-install (e.g. troubleshooting a corrupted install) is
still available as an explicit opt-in:

```
deploy.bat -FullReinstall
```

Neither path touches `pgdata\` or `config\` — see
[uninstall.ps1](../scripts/windows/uninstall.ps1)'s own reasoning for why
data always outlives the software that used it. `deploy.bat` accepts a
different MSI path as its first argument if you're not installing the
default `dist\ISP-BSS-Setup-1.0.0.msi`.

## Verifying a build without installing it

Nothing below needs Administrator rights — installing the MSI for real
does.

```powershell
# Structural validation (ICE checks) against the compiled .msi.
wix msi validate dist\ISP-BSS-Setup-1.0.0.msi

# Decompile back to source to inspect the real Directory/File/CustomAction
# tables — confirms what actually got compiled in, independent of what
# Product.wxs says it should have.
wix msi decompile dist\ISP-BSS-Setup-1.0.0.msi -o decompiled.wxs
```

An administrative extraction (`msiexec /a ... TARGETDIR=...`) also works
without elevation and unpacks the real file layout to disk, but needs
real scratch space on top of the MSI itself — it stages through
`%TEMP%\Installer\` the same way `wix build`'s own cabinet step does, so
point `TEMP`/`TMP` somewhere with room first if the default drive is
tight.
