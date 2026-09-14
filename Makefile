BINARY  := taihu
BIN_DIR := bin
MAIN    := ./cmd/taihu

VERSION_PKG := github.com/liucxer/taihu/internal/version
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null)
BUILDTIME   := $(shell git log -1 --format=%cd --date=format:%Y%m%d%H%M 2>/dev/null)
LDFLAGS     := -X '$(VERSION_PKG).Commit=$(COMMIT)' -X '$(VERSION_PKG).BuildTime=$(BUILDTIME)'

.PHONY: all build clean

all: build

build:
	mkdir -p $(BIN_DIR)
	go build -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) $(MAIN)

clean:
	rm -rf $(BIN_DIR)
