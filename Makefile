# messenger-core Makefile
#
# Быстрый старт:
#   make vendor-zt        – скачать и собрать libzt (Linux/macOS native)
#   make vendor-zt-win    – собрать libzt для Windows (нужен mingw-w64)
#   make client           – собрать CLI-клиент
#   make server           – собрать relay-сервер
#   make windows          – собрать Windows .exe
#   make test             – запустить тесты
#   make clean            – очистить артефакты

ZT_LIB     ?= $(CURDIR)/vendor/zerotier/lib
ZT_INCLUDE ?= $(CURDIR)/vendor/zerotier/include

CGO_LDFLAGS_LINUX := -L$(ZT_LIB) -lzerotiercore -lstdc++ -lm
CGO_LDFLAGS_WIN   := -L$(ZT_LIB) -lzerotiercore -lws2_32 -liphlpapi -lshlwapi -static -static-libgcc -static-libstdc++
CGO_CFLAGS_COMMON := -I$(ZT_INCLUDE)

export CGO_CFLAGS := $(CGO_CFLAGS_COMMON)

.PHONY: all client server windows test lint clean vendor-zt vendor-zt-win

all: client server

client:
	CGO_LDFLAGS="$(CGO_LDFLAGS_LINUX)" \
	go build -tags fts5 -o bin/messenger-cli ./cmd/client

server:
	go build -tags fts5 -o bin/messenger-server ./cmd/server

windows:
	CGO_ENABLED=1 \
	GOOS=windows GOARCH=amd64 \
	CC=x86_64-w64-mingw32-gcc \
	CXX=x86_64-w64-mingw32-g++ \
	CGO_LDFLAGS="$(CGO_LDFLAGS_WIN)" \
	go build -tags fts5 -o bin/messenger-cli.exe ./cmd/client

vendor-zt:
	@bash scripts/build_libzt.sh

vendor-zt-win:
	@bash scripts/build_libzt.sh --windows

test:
	go test ./...

lint:
	golangci-lint run ./...

clean:
	rm -rf bin/ build_libzt_tmp/
	go clean ./...
