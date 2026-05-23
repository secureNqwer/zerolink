@echo off
chcp 65001 >nul
title Zerolink Installer
setlocal enabledelayedexpansion

set "REPO=secureNqwer/zerolink"
set "BIN=zerolink"
set "DEST=%LOCALAPPDATA%\Zerolink"

echo ============================================
echo   Zerolink Installer for Windows
echo ============================================
echo.

:: Detect architecture
wmic os get osarchitecture 2>nul | find "64" >nul
if %ERRORLEVEL% neq 0 (
    echo [ERROR] Only 64-bit Windows is supported
    pause
    exit /b 1
)
set "ARCH=amd64"

:: Detect latest release
echo [..] Fetching latest release...
for /f "tokens=*" %%i in ('curl -sL "https://api.github.com/repos/%REPO%/releases/latest" ^| find "tag_name"') do set "LINE=%%i"
for /f "tokens=4 delims=^" %%i in ("%LINE%") do set "TAG=%%i"
if "%TAG%"=="" (
    echo [ERROR] Failed to fetch latest release
    pause
    exit /b 1
)
echo [OK] Latest: %TAG%

:: Download
set "BASENAME=%BIN%-%TAG:v=%-windows-%ARCH%"
set "URL=https://github.com/%REPO%/releases/download/%TAG%/%BASENAME%.zip"
set "TMP=%TEMP%\zerolink-install"

echo [..] Downloading %BASENAME%...
if exist "%TMP%" rmdir /s /q "%TMP%"
mkdir "%TMP%"
curl -sL "%URL%" -o "%TMP%\zerolink.zip"
if %ERRORLEVEL% neq 0 (
    echo [ERROR] Download failed: %URL%
    pause
    exit /b 1
)

:: Extract
echo [..] Extracting...
powershell -Command "Expand-Archive -Path '%TMP%\zerolink.zip' -DestinationPath '%TMP%\' -Force" >nul
if %ERRORLEVEL% neq 0 (
    echo [ERROR] Failed to extract archive
    pause
    exit /b 1
)

:: Install
echo [..] Installing to %DEST%...
if not exist "%DEST%" mkdir "%DEST%"
copy /y "%TMP%\zerolink.exe" "%DEST%\zerolink.exe" >nul

:: Add to PATH
for /f "tokens=2*" %%a in ('reg query "HKCU\Environment" /v PATH 2^>nul') do set "CURRENT_PATH=%%b"
echo %CURRENT_PATH% | find /i "%DEST%" >nul 2>&1
if %ERRORLEVEL% neq 0 (
    reg add "HKCU\Environment" /v PATH /t REG_EXPAND_SZ /d "%CURRENT_PATH%;%DEST%" /f >nul
    echo [OK] Added to PATH. Restart your terminal.
) else (
    echo [OK] Already in PATH.
)

:: Cleanup
rmdir /s /q "%TMP%" >nul 2>&1

echo.
echo ============================================
echo   Zerolink installed!
echo ============================================
echo.
echo   Run: zerolink.exe             — Web UI
echo   Run: zerolink.exe -cli        — Terminal
echo.
pause
