#!/usr/bin/env bash
set -euo pipefail

REPO="secureNqwer/zerolink"
BIN="${BIN:-zerolink}"
PREFIX="${PREFIX:-/usr/local}"

# ── Utils ──────────────────────────────────────────────────────────────────
info()  { printf "\r[ \033[00;34m..\033[0m ] %s\n" "$1"; }
ok()    { printf "\r[ \033[00;32mOK\033[0m ] %s\n" "$1"; }
err()   { printf "\r[ \033[0;31mER\033[0m ] %s\n" "$1"; exit 1; }

# ── Detect platform ────────────────────────────────────────────────────────
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

case "$OS" in
  linux)   OS="linux" ;;
  darwin)  OS="macos" ;;
  mingw*|msys*|cygwin) OS="windows" ;;
  *)       err "unsupported OS: $OS" ;;
esac

case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) err "unsupported arch: $ARCH" ;;
esac

# ── Detect latest release ──────────────────────────────────────────────────
info "Fetching latest release..."
LATEST=$(curl -sL "https://api.github.com/repos/$REPO/releases/latest" | grep '"tag_name"' | cut -d'"' -f4)
TAG="${TAG:-$LATEST}"

# ── Download & install ─────────────────────────────────────────────────────
BASENAME="${BIN}-${TAG#v}-${OS}-${ARCH}"
URL="https://github.com/$REPO/releases/download/$TAG/$BASENAME.tar.gz"

TMP=$(mktemp -d)
trap "rm -rf $TMP" EXIT

info "Downloading $BASENAME..."
curl -sL "$URL" | tar -xz -C "$TMP"

info "Installing to $PREFIX/bin..."
sudo install -d "$PREFIX/bin"
sudo install -m 755 "$TMP/$BIN" "$PREFIX/bin/$BIN"

# Install server binary if present
if [ -f "$TMP/${BIN}-server" ]; then
  sudo install -m 755 "$TMP/${BIN}-server" "$PREFIX/bin/${BIN}-server"
fi

# Install desktop files
if [ -d "$TMP/icons" ]; then
  sudo install -d "$PREFIX/share/icons/hicolor/256x256/apps"
  sudo install -m 644 "$TMP/icons/"* "$PREFIX/share/icons/hicolor/256x256/apps/"
fi
if [ -f "$TMP/zerolink.desktop" ]; then
  sudo install -d "$PREFIX/share/applications"
  sudo install -m 644 "$TMP/zerolink.desktop" "$PREFIX/share/applications/"
fi

ok "Zerolink ${TAG} installed!"
echo "  Run: $BIN              — Web UI (http://localhost:8081)"
echo "  Run: $BIN -cli        — Terminal"
echo "  Run: $BIN -gui        — Desktop"
