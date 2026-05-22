@echo off
chcp 65001 >nul
title Zerolink Installer
echo === Zerolink Installer for Windows ===
echo.

:: Check if Go is installed
where go >nul 2>&1
if %ERRORLEVEL% neq 0 (
    echo [INFO] Installing Go via winget...
    winget install GoLang.Go -e --silent
    :: Refresh PATH
    call set PATH=%PATH%;%USERPROFILE%\AppData\Local\Programs\Go\bin
)

:: Check if git is installed
where git >nul 2>&1
if %ERRORLEVEL% neq 0 (
    echo [INFO] Installing Git...
    winget install Git.Git -e --silent
)

:: Check if cmake is installed
where cmake >nul 2>&1
if %ERRORLEVEL% neq 0 (
    echo [INFO] Installing CMake...
    winget install Kitware.CMake -e --silent
)

:: Check if MinGW is installed (for CGO)
where gcc >nul 2>&1
if %ERRORLEVEL% neq 0 (
    echo [INFO] Installing MinGW-w64...
    winget install GNU.Mingw-w64 -e --silent
)

set DIR=%USERPROFILE%\zerolink

:: Clone or update
if exist "%DIR%\.git" (
    echo [INFO] Updating repository...
    cd /d "%DIR%"
    git pull --ff-only
) else (
    echo [INFO] Cloning repository...
    git clone https://github.com/secureNqwer/zerolink.git "%DIR%"
    cd /d "%DIR%"
)

:: Build
echo [INFO] Building Zerolink...
go build -tags fts5 -o zerolink.exe ./cmd/client

echo.
echo ✓ Zerolink built successfully!
echo.
echo   Binary: %DIR%\zerolink.exe
echo.
echo   To run: zerolink.exe -gui
echo   To update: cd %DIR% ^&^& git pull ^&^& go build -tags fts5 -o zerolink.exe ./cmd/client
echo.

pause
