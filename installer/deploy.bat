@echo off
rem deploy.bat - installs/upgrades ISP BSS/OSS from the built MSI in one
rem step (stop services, msiexec, verify) via deploy.ps1. Elevates itself
rem through UAC if not already running as Administrator - only *installing*
rem needs elevation, the same split build.bat/build.ps1 already draw for
rem *building* needing none.
rem
rem A separate elevated window opens to do the real work; this window then
rem closes - that is how self-elevation works, not a bug in this script.
rem
rem Usage:
rem   deploy.bat
rem       Installs dist\ISP-BSS-Setup-1.0.0.msi as an in-place upgrade.
rem   deploy.bat "D:\path\to\some.msi"
rem       Installs a specific MSI instead.
rem   deploy.bat "" -FullReinstall
rem       Uninstalls whatever is currently registered first, then installs
rem       dist\ISP-BSS-Setup-1.0.0.msi. See deploy.ps1's own comment on why
rem       this is opt-in rather than the default.
rem   deploy.bat "D:\path\to\some.msi" -FullReinstall
rem       Both together.

setlocal

set "MSI_PATH=%~1"
set "EXTRA_ARGS=%~2"

rem 'net session' only succeeds when the current process is elevated -
rem the standard no-extra-tools way to check from plain batch.
net session >nul 2>&1
if %ERRORLEVEL% NEQ 0 (
    echo Requesting Administrator privileges - a new window will open...
    powershell -NoProfile -Command "Start-Process -FilePath '%~f0' -ArgumentList '\"%MSI_PATH%\" %EXTRA_ARGS%' -Verb RunAs"
    exit /b
)

if "%MSI_PATH%"=="" set "MSI_PATH=%~dp0dist\ISP-BSS-Setup-1.0.0.msi"

powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0deploy.ps1" -MsiPath "%MSI_PATH%" %EXTRA_ARGS%
set "EXIT_CODE=%ERRORLEVEL%"

echo.
pause
exit /b %EXIT_CODE%
