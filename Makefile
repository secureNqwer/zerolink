# Zerolink Makefile
APP_NAME   := zerolink
VERSION    := $(shell git describe --tags --always 2>/dev/null || echo "1.0.0")
COMMIT     := $(shell git log --format="%h" -1 2>/dev/null || echo "dev")
BUILDTIME  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -ldflags "-X github.com/secureNqwer/zerolink/version.Version=$(VERSION) -X github.com/secureNqwer/zerolink/version.Commit=$(COMMIT) -X github.com/secureNqwer/zerolink/version.BuildTime=$(BUILDTIME)"

ZT_LIB     ?= $(CURDIR)/vendor/zerotier/lib
ZT_INCLUDE ?= $(CURDIR)/vendor/zerotier/include

CGO_LDFLAGS_LINUX := -L$(ZT_LIB) -lzerotiercore -lstdc++ -lm
CGO_LDFLAGS_WIN   := -L$(ZT_LIB) -lzerotiercore -lws2_32 -liphlpapi -lshlwapi -static -static-libgcc -static-libstdc++
CGO_CFLAGS_COMMON := -I$(ZT_INCLUDE)

export CGO_CFLAGS := $(CGO_CFLAGS_COMMON)

.PHONY: all client server windows test lint clean install uninstall release vendor-zt vendor-zt-win

all: client server

client:
	CGO_LDFLAGS="$(CGO_LDFLAGS_LINUX)" \
	go build -tags fts5 $(LDFLAGS) -o bin/$(APP_NAME) ./cmd/client

server:
	go build -tags fts5 $(LDFLAGS) -o bin/$(APP_NAME)-server ./cmd/server

windows:
	CGO_ENABLED=1 \
	GOOS=windows GOARCH=amd64 \
	CC=x86_64-w64-mingw32-gcc \
	CXX=x86_64-w64-mingw32-g++ \
	CGO_LDFLAGS="$(CGO_LDFLAGS_WIN)" \
	go build -tags fts5 $(LDFLAGS) -o bin/$(APP_NAME).exe ./cmd/client

# System installation
PREFIX ?= /usr/local
install: client
	install -d $(DESTDIR)$(PREFIX)/bin
	install -d $(DESTDIR)$(PREFIX)/share/icons/hicolor/256x256/apps
	install -m 755 bin/$(APP_NAME) $(DESTDIR)$(PREFIX)/bin/$(APP_NAME)
	install -m 755 bin/$(APP_NAME)-server $(DESTDIR)$(PREFIX)/bin/$(APP_NAME)-server
	install -m 644 icons/$(APP_NAME).png $(DESTDIR)$(PREFIX)/share/icons/hicolor/256x256/apps/$(APP_NAME).png
	@echo "Installed to $(DESTDIR)$(PREFIX)/bin/"
	# Desktop entry
	install -d $(DESTDIR)$(PREFIX)/share/applications
	sed "s|EXEC|$(PREFIX)/bin/$(APP_NAME)|g; s|ICON|$(APP_NAME)|g" < zerolink.desktop.in > $(DESTDIR)$(PREFIX)/share/applications/zerolink.desktop 2>/dev/null || true
	@echo "Desktop entry created"

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/$(APP_NAME)
	rm -f $(DESTDIR)$(PREFIX)/bin/$(APP_NAME)-server
	rm -f $(DESTDIR)$(PREFIX)/share/applications/zerolink.desktop
	rm -f $(DESTDIR)$(PREFIX)/share/icons/hicolor/256x256/apps/$(APP_NAME).png

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
