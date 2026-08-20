@echo off
rem build.bat - convenience wrapper around build.ps1 for this machine's
rem toolchain layout: the .NET SDK and WiX CLI were installed portably
rem under D:\ (dotnet-install.ps1 -InstallDir D:\dotnet-sdk, then
rem dotnet tool install --tool-path D:\wix-tools --version 5.0.2 wix)
rem rather than to the default C:\ locations, specifically to keep this
rem build off a system drive that has repeatedly run short on room while
rem working on this installer. Building the MSI itself needs no elevation;
rem only *installing* the result does.
rem
rem Usage:
rem   build.bat [PgsqlSource] [Version]
rem     PgsqlSource - path to the extracted PostgreSQL Windows binaries
rem                   zip's root (the dir directly containing bin\, lib\,
rem                   share\). Defaults to the copy already extracted on
rem                   this machine if omitted.
rem     Version     - MSI version, must be bare major.minor.build
rem                   (default: 1.0.0)
rem
rem   build.bat
rem   build.bat D:\pgsql 1.1.0

setlocal

set "PGSQL_SOURCE=%~1"
if "%PGSQL_SOURCE%"=="" set "PGSQL_SOURCE=D:\isp-winbuild\pgtest\pgsql"

set "VERSION=%~2"
if "%VERSION%"=="" set "VERSION=1.0.0"

if not exist "%PGSQL_SOURCE%\bin\initdb.exe" (
    echo.
    echo ERROR: no PostgreSQL binaries found under "%PGSQL_SOURCE%"
    echo Pass the extracted PostgreSQL Windows binaries zip's root as the first argument:
    echo   build.bat D:\path\to\pgsql
    echo.
    exit /b 1
)

set "DOTNET_ROOT=D:\dotnet-sdk"
set "PATH=D:\dotnet-sdk;D:\wix-tools;%PATH%"

rem wix build's cabinet-staging step defaults to a folder under %TEMP% -
rem see build.ps1's own -intermediatefolder note for why that matters on
rem a machine where the system drive is tight. Redirected here too, for
rem the same reason, since this .bat is the entry point that actually runs
rem on that machine.
if not exist "D:\temp" mkdir "D:\temp"
set "TEMP=D:\temp"
set "TMP=D:\temp"

powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0build.ps1" ^
    -PgsqlSource "%PGSQL_SOURCE%" ^
    -Version "%VERSION%" ^
    -WixExe "D:\wix-tools\wix.exe"

set "BUILD_EXIT=%ERRORLEVEL%"
if not "%BUILD_EXIT%"=="0" (
    echo.
    echo build.bat: build.ps1 failed with exit code %BUILD_EXIT%
    exit /b %BUILD_EXIT%
)

echo.
echo build.bat: done -^> %~dp0dist\ISP-BSS-Setup-%VERSION%.msi

endlocal
