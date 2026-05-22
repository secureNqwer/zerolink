#!/usr/bin/env bash
set -euo pipefail

REPO="https://github.com/secureNqwer/zerolink.git"
DIR="${DIR:-$HOME/zerolink}"
PREFIX="${PREFIX:-/usr/local}"

echo "==> Zerolink Installer"
echo ""

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  PLATFORM="linux-amd64" ;;
  aarch64|arm64) PLATFORM="linux-arm64" ;;
  *)       PLATFORM="linux-$ARCH" ;;
esac

# Detect package manager
if command -v apt &>/dev/null; then
  PKG_MAN="apt"
  BUILD_DEPS=$(echo "golang git cmake make gcc g++" && [ "$PLATFORM" = "linux-amd64" ] && echo "webkit2gtk" || echo "")
elif command -v pacman &>/dev/null; then
  PKG_MAN="pacman"
  BUILD_DEPS=$(echo "go git cmake make base-devel" && [ "$PLATFORM" = "linux-amd64" ] && echo "webkit2gtk" || echo "")
elif command -v dnf &>/dev/null; then
  PKG_MAN="dnf"
  BUILD_DEPS=$(echo "golang git cmake make gcc gcc-c++" && echo "")
elif command -v zypper &>/dev/null; then
  PKG_MAN="zypper"
  BUILD_DEPS=$(echo "go git cmake make gcc gcc-c++" && echo "")
else
  echo "Warning: unknown package manager. Install deps manually: go, git, cmake, make, gcc"
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

# Try to download pre-built libzt, fall back to compiling
if [[ ! -f vendor/zerotier/lib/libzt.a ]]; then
  TAG=$(git describe --tags --always 2>/dev/null || echo "latest")
  echo "==> Downloading pre-built libzt for $PLATFORM..."
  if curl -sLf "https://github.com/secureNqwer/zerolink/releases/download/$TAG/libzt-$PLATFORM.tar.gz" -o /tmp/libzt.tar.gz; then
    tar -xzf /tmp/libzt.tar.gz -C vendor 2>/dev/null && echo "  done"
    rm -f /tmp/libzt.tar.gz
  else
    echo "  No pre-built libzt, building from source (5-10 min)..."
    bash scripts/build_libzt.sh
  fi
fi

# Build zerolink
echo "==> Building Zerolink..."
make client

# Install to system
echo "==> Installing to $PREFIX/bin..."
sudo install -d "$PREFIX/bin"
sudo install -d "$PREFIX/share/icons/hicolor/256x256/apps"
sudo install -m 755 bin/zerolink "$PREFIX/bin/zerolink"
sudo install -m 644 icons/zerolink.png "$PREFIX/share/icons/hicolor/256x256/apps/zerolink.png"

# Desktop entry
if [[ -d "$PREFIX/share/applications" ]]; then
  sudo install -d "$PREFIX/share/applications"
  sed "s|EXEC|$PREFIX/bin/zerolink|g; s|ICON|zerolink|g" zerolink.desktop.in | sudo tee "$PREFIX/share/applications/zerolink.desktop" > /dev/null
fi

echo ""
echo "✓ Zerolink installed!"
echo "  Run: zerolink       — Web UI (default)"
echo "  Run: zerolink -gui  — Desktop window"
echo "  Run: zerolink -cli  — Terminal"
echo ""
echo "  To update: cd $DIR && git pull && make client && sudo make install"
