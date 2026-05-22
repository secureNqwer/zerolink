# messenger-core

Полностью самодостаточное ядро мессенджера на Go.
Работает поверх **ZeroTier** через встроенную **libzt** (ZeroTier SDK) —
устанавливать ZeroTier One на машину пользователя **не нужно**.

---

## Архитектура

```
┌─────────────────────────────────────────────────────────────────┐
│                   UI Shell (ваша оболочка)                      │
│         (Flutter / Electron / Qt / Web / CLI / …)               │
└──────────────────────────┬──────────────────────────────────────┘
                           │  core.Messenger
┌──────────────────────────▼──────────────────────────────────────┐
│                    messenger.Engine                             │
│  ┌───────────┐  ┌────────────┐  ┌──────────┐  ┌────────────┐  │
│  │  Crypto   │  │ Transport  │  │ Storage  │  │   Media    │  │
│  │ X25519    │  │ (libzt CGO)│  │ (SQLite) │  │ Processor  │  │
│  │ Ed25519   │  │ UDP socket │  │ Messages │  │ ffmpeg opt │  │
│  │ AES-GCM   │  │ Packet     │  │ Chats    │  ├────────────┤  │
│  │ ChaCha20  │  │ framing    │  │ Peers    │  │    Call    │  │
│  │ HKDF      │  │ zstd/LZ4  │  │ Media    │  │  Manager  │  │
│  │ D-Ratchet │  │ compress   │  │ FTS idx  │  │ WebRTC    │  │
│  │ SenderKey │  └────────────┘  └──────────┘  └────────────┘  │
│  └───────────┘                                                  │
│  ┌───────────────┐  ┌─────────────────┐  ┌──────────────────┐  │
│  │  EventBus     │  │ TransferManager │  │  Server Relay    │  │
│  │  pub-sub      │  │ progress/pause  │  │  (опционально)   │  │
│  └───────────────┘  └─────────────────┘  └──────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
                 ↕ ZeroTier Virtual Network (P2P)
┌─────────────────────────────────────────────────────────────────┐
│             Optional Relay Server (server/)                     │
│  WebSocket │ SQLite storage │ Fan-out │ Media CDN cache         │
└─────────────────────────────────────────────────────────────────┘
```

---

## Структура проекта

```
messenger-core/
├── Makefile
├── README.md
├── go.mod
├── scripts/
│   └── build_libzt.sh       # скрипт сборки libzt
├── vendor/
│   └── zerotier/            # заполняется скриптом
│       ├── include/
│       │   └── ZeroTierSockets.h
│       └── lib/
│           └── libzt.a      (или libzerotiercore.a → symlink)
├── core/
│   ├── types.go             # все типы + интерфейсы
│   └── bus.go               # EventBus
├── crypto/
│   └── crypto.go            # X25519, Ed25519, AES-GCM, Double-Ratchet, SenderKey
├── transport/
│   ├── zerotier.go          # ZTTransport (CGO → libzt)
│   ├── zt_callbacks.c       # C-коллбэки libzt
│   ├── zt_events.go         # Go-сторона event bridge
│   └── compress.go          # zstd / LZ4 / auto-detect
├── storage/
│   └── sqlite.go            # SQLiteStorage + FTS5
├── media/
│   ├── processor.go         # изображения / аудио / видео / файлы
│   └── call.go              # WebRTC CallManager + RTCP stats
├── messenger/
│   ├── engine.go            # Engine — центральная точка
│   ├── server_relay.go      # WS-клиент к relay-серверу
│   └── transfer.go          # TransferManager с progress/pause/resume
├── server/
│   └── server.go            # Relay Server
└── cmd/
    ├── client/main.go       # reference CLI
    └── server/main.go       # relay server binary
```

---

## Сборка — шаг за шагом

### ⚠️ Важно: правильная библиотека

Нужен репозиторий **[zerotier/libzt](https://github.com/zerotier/libzt)** —
это ZeroTier SDK, предназначенный для встраивания в приложения.
**Не** `ZeroTierOne` (это демон для рабочей станции, у него нет `Makefile.core`).

---

### Linux (Ubuntu / Debian)

```bash
# 1. Установить зависимости
sudo apt update
sudo apt install -y git cmake build-essential golang-go

# 2. Собрать libzt
make vendor-zt

# 3. Собрать клиент и сервер
make client server

# Бинарники окажутся в bin/
./bin/messenger-cli -network <ваш_ZT_network_id>
./bin/messenger-server
```

### macOS

```bash
# Homebrew
brew install cmake go

make vendor-zt
make client server
```

### Windows (нативно, через WSL2 или кросс-компиляция)

**Вариант A — кросс-компиляция из Linux (рекомендуется):**

```bash
# Установить mingw-w64
sudo apt install -y mingw-w64

# Собрать libzt под Windows
make vendor-zt-win

# Собрать .exe
make windows
# → bin/messenger-cli.exe
```

**Вариант B — нативно в Windows (MSYS2 / MINGW64):**

Открыть MSYS2 MinGW64 terminal:

```bash
pacman -S --noconfirm mingw-w64-x86_64-cmake \
    mingw-w64-x86_64-gcc mingw-w64-x86_64-go git make

# Собрать libzt
bash scripts/build_libzt.sh

# Собрать клиент
CGO_LDFLAGS="-L$(pwd)/vendor/zerotier/lib -lzerotiercore -lws2_32 -liphlpapi -static-libgcc -static-libstdc++" \
CGO_CFLAGS="-I$(pwd)/vendor/zerotier/include" \
go build -o bin/messenger-cli.exe ./cmd/client
```

> **Примечание про Windows-линковку:**
> libzt на Windows требует дополнительных системных библиотек:
> `-lws2_32` (Winsock2) и `-liphlpapi` (IP Helper API).
> Это уже прописано в `Makefile` и `scripts/build_libzt.sh`.

---

### Ручная сборка libzt (если скрипт не подошёл)

```bash
git clone --depth=1 --branch v1.8.10 https://github.com/zerotier/libzt.git
cd libzt
git submodule update --init --recursive

mkdir build && cd build

# Linux/macOS
cmake .. -DCMAKE_BUILD_TYPE=Release \
         -DBUILD_SHARED_LIBS=OFF \
         -DZTS_ENABLE_PYTHON=OFF \
         -DZTS_ENABLE_JAVA=OFF \
         -DCMAKE_POLICY_VERSION_MINIMUM=3.5
cmake --build . --target zt -j$(nproc)

# Скопировать артефакты
mkdir -p ../../vendor/zerotier/{lib,include}
find . -name 'libzt*.a' | head -1 | xargs -I{} cp {} ../../vendor/zerotier/lib/libzerotiercore.a
cp ../include/ZeroTierSockets.h ../../vendor/zerotier/include/
```

---

## Использование ядра из своей оболочки

```go
package main

import (
    "context"
    "fmt"
    "github.com/yourorg/messenger-core/core"
    "github.com/yourorg/messenger-core/messenger"
)

func main() {
    cfg := core.DefaultConfig()
    cfg.Networks         = []core.NetworkID{"a09acf0233ceff0e"}
    cfg.E2EEnabled       = true
    cfg.RequireContactAccept = true
    cfg.DBPath           = "./my-messenger.db"
    cfg.IdentityFile     = "./identity.json"

    m, err := messenger.New(cfg)
    if err != nil { panic(err) }

    ctx := context.Background()
    m.Start(ctx)
    defer m.Stop()

    // Подписаться на события
    events := m.Events().Subscribe("ui",
        core.EvtMessageReceived,
        core.EvtPeerOnline,
        core.EvtCallIncoming,
        core.EvtTransferProgress,
        core.EvtReactionChanged,
    )
    go func() {
        for evt := range events {
            switch evt.Type {
            case core.EvtMessageReceived:
                msg := evt.Data.(*core.Message)
                fmt.Printf("[%s] %s\n", msg.SenderID.NodeID, msg.Payload)

            case core.EvtCallIncoming:
                sess := evt.Data.(*core.CallSession)
                // Автоматически принять звонок
                m.Calls().AcceptCall(ctx, sess.ID)

            case core.EvtTransferProgress:
                p := evt.Data.(*core.TransferProgress)
                fmt.Printf("transfer %s: %.0f%%\n", p.TransferID, p.Percent)

            case core.EvtReactionChanged:
                d := evt.Data.(map[string]interface{})
                fmt.Printf("reaction %s on msg %s\n", d["emoji"], d["msg_id"])
            }
        }
    }()

    // Создать группу и отправить сообщение
    chat, _ := m.CreateChat("Команда", []core.PeerID{aliceID, bobID})
    m.SendText(ctx, chat.ID, "Всем привет!", nil)

    // Форвард сообщения в другой чат
    m.ForwardMessage(ctx, someMessageID, anotherChatID)

    // Safety Number для проверки личности собеседника
    sn, _ := m.SafetyNumber(aliceID)
    fmt.Println("Safety Number:", sn) // 60-значное число

    // Статистика
    stats := m.Stats()
    fmt.Printf("sent=%d recv=%d calls=%d\n",
        stats.MessagesSent, stats.MessagesReceived, stats.ActiveCalls)

    // Позвонить
    sess, _ := m.Calls().InitiateCall(ctx, chat.ID, core.CallVoice)
    fmt.Println("call session:", sess.ID)

    select {} // держим процесс живым
}
```

---

## Криптография

| Операция | Алгоритм |
|---|---|
| Ключевое соглашение | X25519 ECDH |
| Подпись / идентификация | Ed25519 |
| Шифрование (DM) | AES-256-GCM, Double-Ratchet |
| Шифрование (группы) | Sender Key Protocol (per-member forward secrecy) |
| Быстрое шифрование | ChaCha20-Poly1305 |
| KDF | HKDF-SHA256 |
| Верификация личности | Safety Numbers (Signal-совместимые, 5200 итераций SHA-512) |
| HMAC пакетов | HMAC-SHA256 |
| Предупреждение о смене ключа | `EvtPeerKeyChanged` |

---

## Компрессия

| Алгоритм | Когда используется |
|---|---|
| **Zstandard** | Текст, файлы, JSON |
| **LZ4** | Потоковые медиачанки |
| **None** | JPEG, Opus, H.264, ZIP и другие уже сжатые форматы (auto-detect по magic bytes) |

---

## Быстрый старт (два узла)

```bash
# Узел A
./bin/messenger-cli -network a09acf0233ceff0e

# Внутри:
> /name Alice
> /join a09acf0233ceff0e

# Узел B (другая машина или ВМ)
./bin/messenger-cli -network a09acf0233ceff0e

# Узел A: создать DM
> /dm <nodeID_B>:<fingerprint_B>
> /msg <chatID> Привет!

# Проверить Safety Number
> /safety <nodeID_B>:<fingerprint_B>
```

---

## Опциональный Relay-сервер

```bash
# Запустить сервер
./bin/messenger-server -config server.json
```

```json
{
  "listen_addr": ":8080",
  "db_path": "/var/lib/messenger/server.db",
  "max_msg_age_days": 30,
  "tls_cert": "/etc/letsencrypt/live/example.com/fullchain.pem",
  "tls_key":  "/etc/letsencrypt/live/example.com/privkey.pem"
}
```

```bash
# Клиент с сервером
./bin/messenger-cli \
  -network a09acf0233ceff0e \
  -server relay.example.com:8080
```

Сервер **не видит содержимое** сообщений — всё зашифровано на клиенте.

---

## Возможности

- ✅ P2P через ZeroTier (libzt встроена, ZeroTier One не нужен)
- ✅ E2E: Double-Ratchet (DM) + Sender Key (группы)
- ✅ Safety Numbers для верификации личности
- ✅ Предупреждение при смене ключа собеседника
- ✅ Контактные запросы (фильтрация от незнакомцев)
- ✅ Голос / видео / демонстрация экрана (WebRTC, pion)
- ✅ RTCP статистика качества звонка
- ✅ Изображения, аудио (Opus), видео (H.264), файлы, стикер-паки
- ✅ Форвард сообщений с атрибуцией
- ✅ @упоминания с метаданными
- ✅ Реакции (add/remove/list)
- ✅ Кто прочитал (per-member read receipts)
- ✅ Исчезающие сообщения (per-chat TTL + фоновая очистка)
- ✅ Асинхронные загрузки с прогрессом (pause/resume/cancel)
- ✅ LRU-очистка медиакеша
- ✅ Роли и права в группах (Owner / Admin / Member)
- ✅ Системные сообщения (добавлен/удалён/вышел)
- ✅ Локальный FTS-поиск по расшифрованному тексту (SQLite FTS5)
- ✅ Ротация ключей по времени и числу сообщений
- ✅ Relay-сервер: офлайн-хранение, fan-out, Media CDN, sync
- ✅ Диагностика: `Stats()` с атомарными счётчиками
