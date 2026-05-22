#!/usr/bin/env bash
# build_libzt.sh
# Собирает libzt из репозитория zerotier/libzt (ветка main).
#
# Использование:
#   ./scripts/build_libzt.sh              — native Linux/macOS
#   ./scripts/build_libzt.sh --windows    — кросс-компиляция под Windows (нужен mingw-w64)
#
# После успешного запуска:
#   vendor/zerotier/lib/libzt.a           — статическая библиотека
#   vendor/zerotier/lib/libzerotiercore.a — симлинк (для CGO)
#   vendor/zerotier/include/ZeroTierSockets.h

set -euo pipefail

LIBZT_REPO="https://github.com/zerotier/libzt.git"
BUILD_DIR="$(pwd)/build_libzt_tmp"
VENDOR_LIB="$(pwd)/vendor/zerotier/lib"
VENDOR_INC="$(pwd)/vendor/zerotier/include"
JOBS=$(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 4)
WINDOWS=0

# ── Аргументы ─────────────────────────────────────────────────────────────────
for arg in "$@"; do
  case "$arg" in
    --windows|-w) WINDOWS=1 ;;
    --help|-h)
      echo "Usage: $0 [--windows]"
      echo "  (no flag)   — native Linux / macOS"
      echo "  --windows   — cross-compile for Windows (requires mingw-w64)"
      exit 0
      ;;
  esac
done

# ── Зависимости ───────────────────────────────────────────────────────────────
need() { command -v "$1" &>/dev/null || { echo "ERROR: '$1' не найден. Установите его."; exit 1; }; }
need git
need cmake

if [[ $WINDOWS -eq 1 ]]; then
  need x86_64-w64-mingw32-g++
  need x86_64-w64-mingw32-gcc
  need x86_64-w64-mingw32-ar
  echo "==> Режим: кросс-компиляция для Windows (mingw-w64)"
else
  echo "==> Режим: native $(uname -s)"
fi

# ── Клонирование ──────────────────────────────────────────────────────────────
if [[ ! -d "$BUILD_DIR/.git" ]]; then
  echo "==> Клонирование zerotier/libzt (main)..."
  git clone --depth=1 "$LIBZT_REPO" "$BUILD_DIR"
else
  echo "==> Используется существующая копия: $BUILD_DIR"
  git -C "$BUILD_DIR" pull --ff-only || true
fi

echo "==> Инициализация подмодулей..."
git -C "$BUILD_DIR" submodule update --init --recursive --depth=1

echo "==> Сброс локальных изменений для обеспечения чистоты патчей..."
git -C "$BUILD_DIR" reset --hard
git -C "$BUILD_DIR" submodule foreach --recursive git reset --hard

# ── Патчи для всех платформ ───────────────────────────────────────────────
sed -i 's/\/EHsc //g' "$BUILD_DIR/CMakeLists.txt"
sed -i 's|include_directories(${ZTO_SRC_DIR}/osdep)|include_directories(${ZTO_SRC_DIR}/osdep)\ninclude_directories(${ZTO_SRC_DIR}/ext)|g' "$BUILD_DIR/CMakeLists.txt"

sed -i 's/\&timeout, sizeof(struct timeval)/(const char *)\&timeout, sizeof(struct timeval)/g' "$BUILD_DIR/ext/ZeroTierOne/ext/miniupnpc/connecthostport.c" 2>/dev/null || true
sed -i '1s/^/#include <stdexcept>\n/' "$BUILD_DIR/ext/ZeroTierOne/ext/prometheus-cpp-lite-1.0/core/include/prometheus/registry.h" 2>/dev/null || true
sed -i '1s/^/#include <stdexcept>\n/' "$BUILD_DIR/ext/ZeroTierOne/ext/prometheus-cpp-lite-1.0/core/include/prometheus/family.h" 2>/dev/null || true
sed -i 's/const struct zts_in6_addr zts_in6addr_any = ZTS_IN6ADDR_ANY_INIT;/extern const struct zts_in6_addr zts_in6addr_any;/g' "$BUILD_DIR/include/ZeroTierSockets.h" 2>/dev/null || true
sed -i 's/#ifdef ADD_EXPORTS/#ifdef ZTS_STATIC\n#define ZTS_API\n#elif defined(ADD_EXPORTS)/g' "$BUILD_DIR/include/ZeroTierSockets.h" 2>/dev/null || true

# Windows-specific patches
if [[ $WINDOWS -eq 1 ]]; then
    sed -i 's/Metrics::udp_send += len;/#ifndef _WIN32\n\t\t\tMetrics::udp_send += len;\n#endif/g' "$BUILD_DIR/ext/ZeroTierOne/osdep/Phy.hpp" 2>/dev/null || true
fi

# ── CMake конфигурация ────────────────────────────────────────────────────────
CMAKE_BUILD="$BUILD_DIR/_build"
rm -rf "$CMAKE_BUILD"
mkdir -p "$CMAKE_BUILD"

# Общие флаги — отключаем всё лишнее
CMAKE_ARGS=(
  -DCMAKE_BUILD_TYPE=Release
  -DBUILD_SHARED_LIBS=OFF
  -DZTS_ENABLE_PYTHON=OFF
  -DZTS_ENABLE_JAVA=OFF
  -DZTS_ENABLE_PINVOKE=OFF
  -DZTS_DISABLE_CENTRAL_API=ON
  -DZTS_ENABLE_CENTRAL_API=OFF
  -DBUILD_HOST_SELFTEST=OFF
  # Подавляем предупреждение о старом cmake_minimum_required
  -DCMAKE_POLICY_VERSION_MINIMUM=3.5
)

if [[ $WINDOWS -eq 1 ]]; then
  # Создаём toolchain-файл на лету — не зависим от содержимого репо
  TOOLCHAIN="$BUILD_DIR/_toolchain_mingw64.cmake"
  cat > "$TOOLCHAIN" << 'TC'
set(CMAKE_SYSTEM_NAME Windows)
set(CMAKE_SYSTEM_PROCESSOR x86_64)
set(CMAKE_C_COMPILER   x86_64-w64-mingw32-gcc)
set(CMAKE_CXX_COMPILER x86_64-w64-mingw32-g++)
set(CMAKE_AR           x86_64-w64-mingw32-ar CACHE FILEPATH "")
set(CMAKE_RANLIB       x86_64-w64-mingw32-ranlib)
set(CMAKE_RC_COMPILER  x86_64-w64-mingw32-windres)
set(CMAKE_FIND_ROOT_PATH /usr/x86_64-w64-mingw32)
set(CMAKE_FIND_ROOT_PATH_MODE_PROGRAM NEVER)
set(CMAKE_FIND_ROOT_PATH_MODE_LIBRARY ONLY)
set(CMAKE_FIND_ROOT_PATH_MODE_INCLUDE ONLY)
TC
  CMAKE_ARGS+=(-DCMAKE_TOOLCHAIN_FILE="$TOOLCHAIN")
fi

echo "==> Конфигурация CMake..."
cmake -S "$BUILD_DIR" -B "$CMAKE_BUILD" "${CMAKE_ARGS[@]}"

# ── Сборка ────────────────────────────────────────────────────────────────────
echo "==> Сборка (${JOBS} потоков)..."

# Имя цели в libzt — "zt" или "zt-static" в зависимости от версии.
# Пробуем оба варианта, если первый не существует.
if cmake --build "$CMAKE_BUILD" --target zt --config Release -j "$JOBS" 2>/dev/null; then
  echo "==> Цель 'zt' собрана."
else
  echo "==> Цель 'zt' не найдена, пробуем ALL..."
  cmake --build "$CMAKE_BUILD" --config Release -j "$JOBS"
fi

# ── Поиск артефактов ──────────────────────────────────────────────────────────
# libzt может назвать библиотеку по-разному в зависимости от версии
LIB_FILE=""
for name in libzt.a libzt-static.a libZeroTierSockets.a libzt.lib zt.a libzt_pic.a; do
  found=$(find "$CMAKE_BUILD" -name "$name" 2>/dev/null | head -1)
  if [[ -n "$found" ]]; then
    LIB_FILE="$found"
    echo "==> Найдена библиотека: $LIB_FILE"
    break
  fi
done

# If not found, try building zt-static target explicitly (some platforms only build PIC)
if [[ -z "$LIB_FILE" ]]; then
  echo "==> Попытка собрать zt-static отдельно..."
  if cmake --build "$CMAKE_BUILD" --target zt-static --config Release -j "$JOBS" 2>/dev/null; then
    for name in libzt.a libzt-static.a libzt_pic.a; do
      found=$(find "$CMAKE_BUILD" -name "$name" 2>/dev/null | head -1)
      if [[ -n "$found" ]]; then
        LIB_FILE="$found"
        echo "==> Найдена библиотека: $LIB_FILE"
        break
      fi
    done
  fi
fi

if [[ -z "$LIB_FILE" ]]; then
  echo ""
  echo "ERROR: статическая библиотека не найдена. Содержимое build-директории:"
  find "$CMAKE_BUILD" \( -name '*.a' -o -name '*.lib' \) 2>/dev/null | head -20
  echo ""
  echo "Попробуйте собрать вручную:"
  echo "  cd $BUILD_DIR && bash build.sh host release"
  exit 1
fi

# ── Заголовочный файл ─────────────────────────────────────────────────────────
HEADER=""
for hpath in \
  "$BUILD_DIR/include/ZeroTierSockets.h" \
  "$BUILD_DIR/src/include/ZeroTierSockets.h" \
  "$BUILD_DIR/ZeroTierSockets.h"; do
  if [[ -f "$hpath" ]]; then
    HEADER="$hpath"
    break
  fi
done

if [[ -z "$HEADER" ]]; then
  HEADER=$(find "$BUILD_DIR" -name 'ZeroTierSockets.h' 2>/dev/null | head -1)
fi

if [[ -z "$HEADER" ]]; then
  echo "ERROR: ZeroTierSockets.h не найден."
  exit 1
fi

# ── Копирование в vendor/ ─────────────────────────────────────────────────────
mkdir -p "$VENDOR_LIB" "$VENDOR_INC"

LIBNAME=$(basename "$LIB_FILE")
cp "$LIB_FILE" "$VENDOR_LIB/$LIBNAME"

# На Termux/Android библиотека может быть разделена на libzt_pic.a + libzto_pic.a
# Объединяем их в одну libzerotiercore.a
if [[ "$LIBNAME" == "libzt_pic.a" ]]; then
  ZTO_LIB=$(find "$CMAKE_BUILD" -name "libzto_pic.a" 2>/dev/null | head -1)
  if [[ -n "$ZTO_LIB" ]]; then
    echo "==> Объединение libzt_pic.a + libzto_pic.a → libzerotiercore.a"
    cp "$LIB_FILE" "$VENDOR_LIB/libzerotiercore.a"
    # Извлекаем объекты из второй библиотеки и добавляем их
    TMPDIR=$(mktemp -d)
    cd "$TMPDIR"
    ar x "$ZTO_LIB"
    ar r "$VENDOR_LIB/libzerotiercore.a" *.o 2>/dev/null
    cd /
    rm -rf "$TMPDIR"
    echo "==> Готово: libzerotiercore.a объединена"
  else
    ln -sf "$LIBNAME" "$VENDOR_LIB/libzerotiercore.a"
  fi
else
  # Симлинк libzerotiercore.a → реальная либа
  if [[ "$LIBNAME" != "libzerotiercore.a" ]]; then
    ln -sf "$LIBNAME" "$VENDOR_LIB/libzerotiercore.a"
  fi
fi

cp "$HEADER" "$VENDOR_INC/ZeroTierSockets.h"

# ── Итог ──────────────────────────────────────────────────────────────────────
echo ""
echo "✓  libzt успешно собрана!"
echo ""
echo "   $VENDOR_LIB/$LIBNAME"
echo "   $VENDOR_LIB/libzerotiercore.a  (symlink)"
echo "   $VENDOR_INC/ZeroTierSockets.h"
echo ""

if [[ $WINDOWS -eq 1 ]]; then
  echo "─── Сборка Windows .exe ──────────────────────────────────────────────"
  echo ""
  echo "  CGO_ENABLED=1 \\"
  echo "  GOOS=windows GOARCH=amd64 \\"
  echo "  CC=x86_64-w64-mingw32-gcc \\"
  echo "  CXX=x86_64-w64-mingw32-g++ \\"
  echo "  CGO_LDFLAGS=\"-L\$(pwd)/vendor/zerotier/lib -lzerotiercore -lws2_32 -liphlpapi -lshlwapi -static -static-libgcc -static-libstdc++\" \\"
  echo "  CGO_CFLAGS=\"-I\$(pwd)/vendor/zerotier/include\" \\"
  echo "  go build -o bin/messenger-cli.exe ./cmd/client"
else
  echo "─── Сборка Linux/macOS ───────────────────────────────────────────────"
  echo ""
  echo "  CGO_LDFLAGS=\"-L\$(pwd)/vendor/zerotier/lib -lzerotiercore -lstdc++ -lm\" \\"
  echo "  CGO_CFLAGS=\"-I\$(pwd)/vendor/zerotier/include\" \\"
  echo "  go build -o bin/messenger-cli ./cmd/client"
fi
echo ""
echo "  Или просто: make client   /   make windows"
