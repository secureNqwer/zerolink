@echo off
chcp 65001 >nul
title Zerolink Installer
echo ============================================
echo   Zerolink Installer for Windows
echo ============================================
echo.

:: Change to the directory containing this script
cd /d "%~dp0"
echo [INFO] Working directory: %~dp0
echo.

:: -----------------------------------------------
:: Check Go (required)
:: -----------------------------------------------
where go >nul 2>&1
if %ERRORLEVEL% neq 0 (
    echo [INFO] Go not found. Attempting to install via winget...
    winget install GoLang.Go -e --silent
    if %ERRORLEVEL% neq 0 (
        echo [ERROR] Failed to install Go automatically.
        echo         Please install Go manually from https://go.dev/dl/
        echo         Then re-run this script.
        pause
        exit /b 1
    )
    :: Refresh PATH in current session
    set "PATH=%PATH%;%LOCALAPPDATA%\Programs\Go\bin;%ProgramFiles%\Go\bin"
    where go >nul 2>&1
    if %ERRORLEVEL% neq 0 (
        echo [WARNING] Go installed but not yet on PATH in this session.
        echo           Please close and reopen this window, then run the script again.
        pause
        exit /b 1
    )
)
for /f "tokens=*" %%v in ('go version 2^>nul') do echo [OK] %%v

:: -----------------------------------------------
:: Check Git (optional, for updates)
:: -----------------------------------------------
where git >nul 2>&1
if %ERRORLEVEL% neq 0 (
    echo [INFO] Git not found. Installing via winget...
    winget install Git.Git -e --silent
    if %ERRORLEVEL% neq 0 (
        echo [WARNING] Failed to install Git automatically.
        echo           Git is optional — install from https://git-scm.com if you need updates.
    ) else (
        set "PATH=%PATH%;%ProgramFiles%\Git\cmd"
        echo [OK] Git installed.
    )
) else (
    for /f "tokens=*" %%v in ('git --version 2^>nul') do echo [OK] %%v
)

:: -----------------------------------------------
:: Check CMake (required for libzt build)
:: -----------------------------------------------
where cmake >nul 2>&1
if %ERRORLEVEL% neq 0 (
    echo [INFO] CMake not found. Installing via winget...
    winget install Kitware.CMake -e --silent
    if %ERRORLEVEL% neq 0 (
        echo [WARNING] Failed to install CMake automatically.
        echo           Please install from https://cmake.org/download/
    ) else (
        set "PATH=%PATH%;%ProgramFiles%\CMake\bin"
        echo [OK] CMake installed.
    )
) else (
    for /f "tokens=*" %%v in ('cmake --version 2^>nul') do echo [OK] %%v & goto cmake_ok
    :cmake_ok
)

:: -----------------------------------------------
:: Check GCC / MinGW-w64 (required for CGO)
:: -----------------------------------------------
where gcc >nul 2>&1
if %ERRORLEVEL% neq 0 (
    echo [INFO] GCC not found. Installing MinGW-w64 via winget...
    winget install GNU.Mingw-w64 -e --silent
    if %ERRORLEVEL% neq 0 (
        echo [WARNING] Failed to install MinGW automatically.
        echo           Please install MSYS2/MinGW from https://www.msys2.org/
        echo           and make sure gcc.exe is on your PATH.
    ) else (
        :: Common MinGW paths
        set "PATH=%PATH%;%ProgramFiles%\mingw-w64\mingw64\bin;C:\msys64\mingw64\bin"
        where gcc >nul 2>&1
        if %ERRORLEVEL% neq 0 (
            echo [WARNING] MinGW installed but gcc not yet on PATH.
            echo           Add MinGW bin directory to PATH and re-run.
        ) else (
            for /f "tokens=*" %%v in ('gcc --version 2^>nul') do echo [OK] %%v & goto gcc_ok
        )
    )
) else (
    for /f "tokens=*" %%v in ('gcc --version 2^>nul') do echo [OK] %%v & goto gcc_ok
)
:gcc_ok

echo.
echo ============================================
echo   Building Zerolink...
echo ============================================
echo.

:: Enable CGO (required for sqlite fts5)
set CGO_ENABLED=1

if not exist "%~dp0bin" mkdir "%~dp0bin"
go build -tags fts5 -o bin\zerolink.exe ./cmd/client
if %ERRORLEVEL% neq 0 (
    echo.
    echo [ERROR] Build failed! Check the errors above.
    echo         Common fixes:
    echo          - Make sure GCC/MinGW is on PATH (required for CGO)
    echo          - Make sure CMake is installed (required for libzt)
    echo          - Run 'go mod download' if you see missing module errors
    pause
    exit /b 1
)

echo.
echo ============================================
echo   Build successful!
echo ============================================
echo.
echo   Binary: %~dp0bin\zerolink.exe
echo.

:: -----------------------------------------------
:: Optional: Install system-wide
:: -----------------------------------------------
set /p INSTALL_GLOBAL="Install system-wide to %%LOCALAPPDATA%%\Zerolink ? [y/N]: "
if /i "%INSTALL_GLOBAL%"=="y" (
    set "DEST=%LOCALAPPDATA%\Zerolink"
    if not exist "%DEST%" mkdir "%DEST%"
    copy /y "%~dp0bin\zerolink.exe" "%DEST%\zerolink.exe" >nul
    echo [OK] Copied to %DEST%\zerolink.exe

    :: Add to user PATH via registry (persistent)
    for /f "tokens=2*" %%a in ('reg query "HKCU\Environment" /v PATH 2^>nul') do set "CURRENT_PATH=%%b"
    echo %CURRENT_PATH% | find /i "%DEST%" >nul 2>&1
    if %ERRORLEVEL% neq 0 (
        reg add "HKCU\Environment" /v PATH /t REG_EXPAND_SZ /d "%CURRENT_PATH%;%DEST%" /f >nul
        echo [OK] Added to user PATH. Restart your shell to use 'zerolink' globally.
    ) else (
        echo [INFO] Already in user PATH.
    )
)

echo.
echo   To run (Web UI):       bin\zerolink.exe
echo   To run (desktop GUI):  bin\zerolink.exe -gui
echo   To run (CLI mode):     bin\zerolink.exe -cli
echo   To update: cd "%~dp0" ^&^& git pull ^&^& go build -tags fts5 -o bin\zerolink.exe ./cmd/client
echo.
pause
