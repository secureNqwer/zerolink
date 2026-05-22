# Maintainer: secureNqwer
pkgname=zerolink
pkgver=1.0.0
pkgrel=1
pkgdesc="Secure P2P Messenger with ZeroTier"
arch=("x86_64")
url="https://github.com/secureNqwer/messenger-core"
license=("MIT")
source=("https://github.com/secureNqwer/messenger-core/releases/download/v$pkgver/zerolink-linux-amd64"
        "https://github.com/secureNqwer/messenger-core/releases/download/v$pkgver/zerolink-server-linux-amd64")
sha256sums=("SKIP" "SKIP")

package() {
  install -Dm755 zerolink-linux-amd64 "$pkgdir/usr/bin/zerolink"
  install -Dm755 zerolink-server-linux-amd64 "$pkgdir/usr/bin/zerolink-server"
  install -Dm644 "$srcdir/zerolink.desktop" "$pkgdir/usr/share/applications/zerolink.desktop"
}
