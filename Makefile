.PHONY: help build install demo demo-verbose test test-race test-short test-e2e \
        test-coverage fmt fmt-check vet lint lint-install tidy check ci deps clean

BINARY   := hotreload
BIN_DIR  := bin
MAIN_PKG := ./cmd/hotreload

# $(CURDIR) is a GNU make builtin, so no shell call is needed. `pwd` would not
# exist on a Windows command prompt.
ROOT_DIR := $(CURDIR)

# Version stamping. Override on the command line:
#   make build VERSION=1.2.3
VERSION ?= dev
COMMIT  ?= none
DATE    ?= unknown

# Keep in step with the version pinned in .github/workflows/ci.yml.
GOLANGCI_VERSION ?= v2.8.0
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

# Windows needs a .exe suffix, and its shell has different delete syntax.
# Everything else here is portable: `go build -o` creates missing parent
# directories itself, so no mkdir is required.
ifeq ($(OS),Windows_NT)
  EXT     := .exe
  RM_DIR   = if exist "$(1)" rmdir /s /q "$(1)"
  RM_FILE  = if exist "$(1)" del /q "$(1)"
else
  EXT     :=
  RM_DIR   = rm -rf "$(1)"
  RM_FILE  = rm -f "$(1)"
endif

BINARY_PATH     := $(BIN_DIR)/$(BINARY)$(EXT)
TESTSERVER_PATH := $(BIN_DIR)/testserver$(EXT)

## ─── Help ────────────────────────────────────────────────────────────────────

## Show the available targets
help:
	@echo "hotreload make targets:"
	@echo "  build          Build ./$(BINARY_PATH)"
	@echo "  install        go install the CLI"
	@echo "  demo           Build and run the demo against ./testserver"
	@echo "  test           Run unit tests"
	@echo "  test-short     Run unit tests, skipping the slow ones"
	@echo "  test-e2e       Run only the end-to-end tests"
	@echo "  test-race      Run tests under the race detector"
	@echo "  test-coverage  Write coverage.out and coverage.html"
	@echo "  fmt            Format the tree with gofmt"
	@echo "  fmt-check      Fail if anything is unformatted"
	@echo "  vet            Run go vet"
	@echo "  lint           Run golangci-lint"
	@echo "  lint-install   Install the pinned golangci-lint version"
	@echo "  check          fmt-check + vet + test"
	@echo "  ci             What CI runs: fmt-check + vet + test-race"
	@echo "  clean          Remove build artefacts"
	@echo ""
	@echo "On Windows without make, use .\\make.ps1 <target> instead."

## ─── Build ───────────────────────────────────────────────────────────────────

## Build the hotreload binary
build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY_PATH) $(MAIN_PKG)

## Install hotreload into $GOPATH/bin (usually ~/go/bin)
install:
	go install -ldflags "$(LDFLAGS)" $(MAIN_PKG)

## ─── Demo ────────────────────────────────────────────────────────────────────
##
## Run `make demo` then open http://localhost:8080 in a browser.
## Edit the `version` constant in testserver/main.go, save, and watch
## the server restart automatically with the new value.
##
demo: build
	@echo ""
	@echo "=========================================="
	@echo "  hotreload demo"
	@echo "  Edit testserver/main.go and save."
	@echo "  Visit http://localhost:8080"
	@echo "=========================================="
	@echo ""
	$(BINARY_PATH) \
		--root        $(ROOT_DIR)/testserver \
		--build       "go build -C $(ROOT_DIR)/testserver -o $(ROOT_DIR)/$(TESTSERVER_PATH) ." \
		--exec        "$(ROOT_DIR)/$(TESTSERVER_PATH)" \
		--include-ext .go

## Same as demo but with verbose / debug logging
demo-verbose: build
	$(BINARY_PATH) \
		--root        $(ROOT_DIR)/testserver \
		--build       "go build -C $(ROOT_DIR)/testserver -o $(ROOT_DIR)/$(TESTSERVER_PATH) ." \
		--exec        "$(ROOT_DIR)/$(TESTSERVER_PATH)" \
		--include-ext .go \
		--verbose

## ─── Tests ───────────────────────────────────────────────────────────────────

## Run all tests (including the slow end-to-end ones)
test:
	go test ./... -count=1

## Run only the fast tests (skips end-to-end and stubborn-process tests)
test-short:
	go test -short ./... -count=1

## Run only the end-to-end tests
test-e2e:
	go test ./e2e/ -count=1 -v -timeout 15m

## Run all tests with the race detector.
## Needs cgo and a 64-bit C toolchain; on Windows use WSL or rely on CI.
test-race:
	go test -race ./... -count=1 -timeout 20m

## Generate a coverage report
test-coverage:
	go test ./... -coverprofile=coverage.out -covermode=atomic
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report written to coverage.html"

## ─── Quality ─────────────────────────────────────────────────────────────────

## Format every Go file in the tree
fmt:
	gofmt -w .

## Fail if any file is unformatted (what CI enforces).
## This is the one target that needs a POSIX shell; on a Windows command
## prompt use `.\make.ps1 fmt-check` instead.
fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "These files are not gofmt-clean:"; \
		echo "$$unformatted"; \
		echo "Run 'make fmt' to fix them."; \
		exit 1; \
	fi

## Run go vet
vet:
	go vet ./...

## Run golangci-lint. Install the version CI uses with:
##   make lint-install
## The v2 module path matters: the old path installs a v1 binary, which cannot
## read the v2 .golangci.yml and fails with a schema error.
lint:
	golangci-lint run

## Install the pinned golangci-lint version
lint-install:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)

## Tidy dependencies
tidy:
	go mod tidy

## The usual pre-commit sweep
check: fmt-check vet test

## What CI runs
ci: fmt-check vet test-race

## ─── Misc ────────────────────────────────────────────────────────────────────

## Download and tidy dependencies
deps:
	go mod tidy
	go mod download

## Remove build artefacts
clean:
	@$(call RM_DIR,$(BIN_DIR))
	@$(call RM_FILE,coverage.out)
	@$(call RM_FILE,coverage.html)
