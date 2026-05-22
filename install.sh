#!/usr/bin/env bash
set -euo pipefail

REPO="https://github.com/secureNqwer/zerolink.git"
DIR="${DIR:-$HOME/zerolink}"
PREFIX="${PREFIX:-/usr/local}"

echo "==> Zerolink Installer"
echo ""

# Detect package manager
if command -v apt &>/dev/null; then
  PKG_MAN="apt"
  BUILD_DEPS="golang git cmake make gcc g++"
elif command -v pacman &>/dev/null; then
  PKG_MAN="pacman"
  BUILD_DEPS="go git cmake make base-devel"
elif command -v dnf &>/dev/null; then
  PKG_MAN="dnf"
  BUILD_DEPS="golang git cmake make gcc gcc-c++"
elif command -v zypper &>/dev/null; then
  PKG_MAN="zypper"
  BUILD_DEPS="go git cmake make gcc gcc-c++"
else
  echo "Warning: unknown package manager. Install dependencies manually: go, git, cmake, make, gcc/g++"
fi

# Install build dependencies
if [[ -n "${PKG_MAN:-}" ]]; then
  echo "==> Installing build dependencies ($PKG_MAN)..."
  case $PKG_MAN in
    apt)    sudo apt update && sudo apt install -y $BUILD_DEPS ;;
    pacman) sudo pacman -Sy --noconfirm $BUILD_DEPS ;;
    dnf)    sudo dnf install -y $BUILD_DEPS ;;
    zypper) sudo zypper install -y $BUILD_DEPS ;;
  esac
fi

# Clone or update repo
if [[ -d "$DIR/.git" ]]; then
  echo "==> Updating existing repo..."
  cd "$DIR"
  git pull --ff-only
else
  echo "==> Cloning repository..."
  git clone "$REPO" "$DIR"
  cd "$DIR"
fi

# Build libzt
echo "==> Building ZeroTier library..."
bash scripts/build_libzt.sh

# Build zerolink
echo "==> Building Zerolink..."
make client
make server

# Install to system
echo "==> Installing to $PREFIX/bin..."
sudo install -d "$PREFIX/bin"
sudo install -m 755 bin/zerolink "$PREFIX/bin/zerolink"
sudo install -m 755 bin/zerolink-server "$PREFIX/bin/zerolink-server"

# Desktop entry
if [[ -d "$PREFIX/share/applications" ]]; then
  sudo install -d "$PREFIX/share/applications"
  sed "s|EXEC|$PREFIX/bin/zerolink|g" zerolink.desktop.in | sudo tee "$PREFIX/share/applications/zerolink.desktop" > /dev/null
fi

echo ""
echo "✓ Zerolink installed!"
echo "  Run: zerolink         — CLI mode"
echo "  Run: zerolink -gui    — Web UI"
echo "  Run: zerolink-server  — Relay server"
echo ""
echo "  To update: cd $DIR && git pull && make client && sudo make install"
