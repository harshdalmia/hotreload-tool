.PHONY: build build-testserver demo demo-verbose test test-race test-short test-coverage deps clean install

BINARY          := hotreload
BIN_DIR         := ./bin
TESTSERVER_BIN  := $(BIN_DIR)/testserver
MAIN_PKG        := ./cmd/hotreload

# Resolve to absolute path so the nested `cd testserver` build works correctly.
ROOT_DIR        := $(shell pwd)

## Build the hotreload binary
build:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/$(BINARY) $(MAIN_PKG)

## Install hotreload into $GOPATH/bin (usually ~/go/bin)
install:
	go install $(MAIN_PKG)

## ─── Demo ────────────────────────────────────────────────────────────────────
##
## Run `make demo` then open http://localhost:8080 in a browser.
## Edit the `version` constant in testserver/main.go, save, and watch
## the server restart automatically with the new value.
##
demo: build
	@mkdir -p $(BIN_DIR)
	@echo ""
	@echo "=========================================="
	@echo "  hotreload demo"
	@echo "  Edit testserver/main.go and save."
	@echo "  Visit http://localhost:8080"
	@echo "=========================================="
	@echo ""
	$(BIN_DIR)/$(BINARY) \
		--root  ./testserver \
		--build "cd $(ROOT_DIR)/testserver && go build -o $(ROOT_DIR)/$(TESTSERVER_BIN) ." \
		--exec  "$(ROOT_DIR)/$(TESTSERVER_BIN)"

## Same as demo but with verbose / debug logging
demo-verbose: build
	@mkdir -p $(BIN_DIR)
	$(BIN_DIR)/$(BINARY) \
		--root    ./testserver \
		--build   "cd $(ROOT_DIR)/testserver && go build -o $(ROOT_DIR)/$(TESTSERVER_BIN) ." \
		--exec    "$(ROOT_DIR)/$(TESTSERVER_BIN)" \
		--verbose

## ─── Tests ───────────────────────────────────────────────────────────────────

## Run all tests (including slow ones)
test:
	go test ./... -v -count=1

## Run all tests with the race detector
test-race:
	go test -race ./... -count=1

## Run only the fast tests (skips stubborn-process test which takes ~5 s)
test-short:
	go test -short ./... -v -count=1

## Generate a coverage report
test-coverage:
	go test ./... -coverprofile=coverage.out
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report written to coverage.html"

## ─── Misc ────────────────────────────────────────────────────────────────────

## Download and tidy dependencies
deps:
	go mod tidy
	go mod download

## Remove build artefacts
clean:
	rm -rf $(BIN_DIR) coverage.out coverage.html
