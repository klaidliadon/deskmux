# lginput
#
# Written for POSIX make, not just GNU make. The `make` on a Windows box is
# often BusyBox make, which silently ignores $(shell ...) and $(if ...) rather
# than reporting them -- so nothing here depends on command substitution. The
# binary derives its own version from Go's embedded VCS build info instead.
#
# Recipes stick to single tool invocations so they behave the same under
# cmd.exe and a Unix shell.

BINARY := lginput
PKG    := ./cmd/lginput

# Optional. Set for a tagged release: make build VERSION=v1.2.3
# When empty, the binary reports its VCS revision from Go build info.
VERSION ?=

GOLANGCI_VERSION ?= latest

# An empty VERSION yields -X main.version=, which the linker accepts and the
# binary reads as "fall back to VCS build info".
STAMP = -X main.version=$(VERSION)

ifeq ($(OS),Windows_NT)
	EXE := .exe
	RM  := cmd /C del /Q /F
else
	EXE :=
	RM  := rm -f
endif

BIN     := $(BINARY)$(EXE)
BIN_GUI := $(BINARY)w$(EXE)

.DEFAULT_GOAL := build
.PHONY: build gui all test cover bench lint fmt vet tidy check clean install \
        watch volumekeys probe config version help

## build: compile the console binary
build:
	go build -ldflags "$(STAMP)" -o $(BIN) $(PKG)

## gui: compile a windowless binary, for running as a background task
##
## Pair it with a log file: with no console there is nowhere for stdout to go
## and no way to deliver Ctrl+C.
gui:
	go build -ldflags "$(STAMP) -H=windowsgui" -o $(BIN_GUI) $(PKG)

## all: build both binaries
all: build gui

## test: run the unit tests
test:
	go test ./...

## cover: run tests and open a coverage report
cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

## bench: run benchmarks
bench:
	go test -run=NONE -bench=. -benchmem ./...

## fmt: format the tree
fmt:
	go fmt ./...

## vet: run go vet
##
## unsafeptr is off: COM vtable dispatch and Win32 hook callbacks must convert
## uintptr to unsafe.Pointer, and those point at OS-owned memory the collector
## never manages. .golangci.yml excludes the same check with that reasoning.
vet:
	go vet -unsafeptr=false ./...

## lint: run golangci-lint
##
## Fetched on demand, so nothing needs installing. Override GOLANGCI_VERSION
## to pin a release once one is known good for you.
lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION) run

## tidy: prune and verify module requirements
tidy:
	go mod tidy
	go mod verify

## check: what to run before committing
check: fmt vet test

## install: install into GOBIN
install:
	go install -ldflags "$(STAMP)" $(PKG)

## clean: remove build output
clean:
	-$(RM) $(BIN)
	-$(RM) $(BIN_GUI)
	-$(RM) coverage.out
	go clean

## watch: run the dock watcher
watch: build
	./$(BIN) watch

## volumekeys: run the volume-key daemon
volumekeys: build
	./$(BIN) volumekeys

## probe: dump what the monitor exposes
probe: build
	./$(BIN) probe

## config: write a starter configuration file
config: build
	./$(BIN) config init

## version: report the version the built binary carries
version: build
	./$(BIN) version

## help: list targets
help:
	@go run ./internal/tools/mkhelp Makefile
