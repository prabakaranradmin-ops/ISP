@echo off
rem deploy.bat - installs/upgrades ISP BSS/OSS from the built MSI in one
rem step (stop services, msiexec, verify, open the console) via deploy.ps1.
rem Elevates itself through UAC if not already running as Administrator -
rem only *installing* needs elevation, the same split build.bat/build.ps1
rem already draw for *building* needing none.
rem
rem A separate elevated window opens to do the real work; this window then
rem closes - that is how self-elevation works, not a bug in this script.
rem
rem Usage:
rem   deploy.bat
rem       Installs dist\ISP-BSS-Setup-1.0.0.msi as an in-place upgrade,
rem       then opens the staff console login page in your default browser.
rem   deploy.bat "D:\path\to\some.msi"
rem       Installs a specific MSI instead.
rem   deploy.bat "" -FullReinstall
rem       Uninstalls whatever is currently registered first, then installs
rem       dist\ISP-BSS-Setup-1.0.0.msi. See deploy.ps1's own comment on why
rem       this is opt-in rather than the default.
rem   deploy.bat "" -AppMode
rem       Opens the console as a standalone window (Edge/Chrome --app=,
rem       no address bar or tabs) instead of a normal browser tab - for
rem       showing it to a client as a product, not a website.
rem   deploy.bat "" -FullReinstall -AppMode
rem       Any combination of the two flags together, in either order.
rem   deploy.bat "D:\path\to\some.msi" -AppMode
rem       A specific MSI plus a flag.

setlocal

set "MSI_PATH=%~1"
set "FLAG1=%~2"
set "FLAG2=%~3"

rem 'net session' only succeeds when the current process is elevated -
rem the standard no-extra-tools way to check from plain batch.
net session >nul 2>&1
if %ERRORLEVEL% NEQ 0 (
    echo Requesting Administrator privileges - a new window will open...
    powershell -NoProfile -Command "Start-Process -FilePath '%~f0' -ArgumentList '\"%MSI_PATH%\" %FLAG1% %FLAG2%' -Verb RunAs"
    exit /b
)

if "%MSI_PATH%"=="" set "MSI_PATH=%~dp0dist\ISP-BSS-Setup-1.0.0.msi"

powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0deploy.ps1" -MsiPath "%MSI_PATH%" %FLAG1% %FLAG2%
set "EXIT_CODE=%ERRORLEVEL%"

echo.
pause
exit /b %EXIT_CODE%
