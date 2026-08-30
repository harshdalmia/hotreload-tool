# hotreload

[![CI](https://github.com/harshdalmia/hotreload-tool/actions/workflows/ci.yml/badge.svg)](https://github.com/harshdalmia/hotreload-tool/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/harshdalmia/hotreload-tool)](https://goreportcard.com/report/github.com/harshdalmia/hotreload-tool)
[![Release](https://img.shields.io/github/v/release/harshdalmia/hotreload-tool?sort=semver)](https://github.com/harshdalmia/hotreload-tool/releases)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](./LICENSE)

A hot-reload CLI for Go and other compiled projects: on file change it rebuilds your project and restarts your server.

```bash
hotreload --build "go build -o ./bin/server ./cmd/server" --exec "./bin/server"
```

Architecture and design notes live in [ARCHITECTURE.md](./ARCHITECTURE.md).

## Install

```bash
go install github.com/harshdalmia/hotreload-tool/cmd/hotreload@latest
```

Or grab a binary from the [releases page](https://github.com/harshdalmia/hotreload-tool/releases), or build from source:

```bash
make build          # Linux/macOS
.\make.ps1 build    # Windows
```

## Run the demo in 60 seconds

### Linux/macOS

```bash
make demo
```

### Windows (PowerShell)

```powershell
.\make.ps1 demo
```

Then open `http://localhost:8080`, edit the `version` constant in `testserver/main.go`, save, and watch the server restart with the new value.

Without a task runner:

```bash
go build -o ./bin/hotreload ./cmd/hotreload
./bin/hotreload \
  --root        ./testserver \
  --build       "go build -C ./testserver -o ../bin/testserver ." \
  --exec        "./bin/testserver" \
  --include-ext .go
```

## Usage

```text
hotreload --exec "<cmd>" [--build "<cmd>"] [flags]

  --exec          Command that runs your server.                      Required.
  --build         Command run when a change is detected.              Optional.
  --root          Directory watched recursively.                      Default: .
  --debounce      Quiet window after the last change.                 Default: 150ms
  --kill-delay    Grace period before a process is force-killed.      Default: 5s
  --include-ext   Only rebuild for these extensions, comma-separated. Default: all
  --exclude-dir   Extra directory names to ignore, comma-separated.
  --include-dir   Directory names to watch despite being ignored.
  --config        Path to a config file.                              Default: ./.hotreload.toml
  --verbose       Debug logging.
  --version       Print version information and exit.
```

`--build` and `--exec` are passed to your shell (`sh -c` on Unix, `cmd /S /C` on Windows), so pipes, redirects, globs, and environment variables all work.

### Interpreted projects

`--build` is optional. Python, Node, Ruby and friends have nothing to compile, so a save restarts the process directly:

```bash
hotreload --exec "python app.py" --include-ext .py
hotreload --exec "node server.js" --include-ext .js,.mjs
```

It is still worth putting a syntax check in the build slot, because a failed build leaves the running server untouched. That way a half-finished edit can't take your server down:

```bash
hotreload --build "python -m compileall -q ." --exec "python app.py" --include-ext .py
hotreload --build "node --check server.js"     --exec "node server.js" --include-ext .js
```

### Compiled projects other than Go

```bash
hotreload --build "cargo build"    --exec "./target/debug/server" --include-ext .rs
hotreload --build "npx tsc"        --exec "node dist/server.js"   --include-ext .ts
hotreload --build "mvn -q package" --exec "java -jar target/app.jar" --include-ext .java
```

Build output directories are ignored by default, including `target`, `obj`, `bin`, `dist`, `build` and `coverage`. That matters more than it sounds: build tools rewrite files in their output directory on every run, so watching one turns a single save into an endless rebuild loop.

### Only rebuild for the files that matter

By default any non-ignored file change triggers a rebuild, including documentation and config. Narrow it with `--include-ext`:

```bash
hotreload --build "make build" --exec "./bin/app" --include-ext .go,.tmpl
```

Now editing `README.md` costs nothing and editing a `.go` file reloads.

### Watching a directory that is ignored by default

`bin`, `dist`, `build`, `tmp`, `temp`, `vendor`, `node_modules`, and every dot-directory are skipped so build output cannot trigger a rebuild loop. If your sources genuinely live in one of them, opt back in:

```bash
hotreload --build "make" --exec "./out/app" --include-dir build
```

## Configuration file

If `.hotreload.toml` exists in the working directory it is loaded automatically. Precedence runs defaults, then the config file, then flags you passed explicitly — a flag left at its default never overrides the file.

```toml
root        = "."
build       = "go build -o ./bin/app ./cmd/app"
exec        = "./bin/app"
debounce    = "150ms"
kill_delay  = "5s"
include_ext = [".go", ".tmpl"]
exclude_dir = ["testdata"]
include_dir = []
verbose     = false
```

See [.hotreload.example.toml](./.hotreload.example.toml) for the annotated version. Unknown keys are rejected rather than ignored, so a typo fails loudly instead of silently doing nothing.

## Behaviour worth knowing

**Event storms are collapsed.** A single editor save can emit write, chmod, and two renames; a formatter can touch a whole package. Everything inside the debounce window becomes one rebuild.

**A failed build never disturbs a running server.** Your server keeps serving the last binary that compiled. Fix the error, save, and it recovers.

**The newest save always wins.** A change arriving mid-build cancels that build immediately. If the build had already finished, it declines to swap in its now-stale binary and leaves the running server alone rather than stopping it and having nothing to replace it with.

**A crashed server is restarted.** If your server panics or exits non-zero on its own, hotreload restarts it without waiting for a file change. Three crashes within ten seconds is treated as a crash loop and adds a short backoff so the stack trace stays readable. A successful rebuild clears that history.

**A clean exit is not restarted.** A server that exits with status 0 is assumed to have meant it, so a one-shot `--exec` command does not spin forever.

**Whole process trees are stopped.** Your server's children go with it, so nothing is left holding the port. This matters for wrappers like `npm start` or `mvn exec`, where the real process is a child.

**Build output never triggers a rebuild.** `target`, `obj`, `bin`, `dist`, `build`, `coverage`, `node_modules`, `vendor`, `venv` and `__pycache__` are ignored by default, so a build writing its own artefacts cannot start a loop. Use `--include-dir` if your sources genuinely live in one of them.

**No build step is fine.** Omit `--build` and a change restarts your server directly.

**New directories are picked up while running.** Including ones that arrive already populated — a branch switch, an unzip, a `cp -r` — where the files were created before any watch existed.

**Deleted and recreated directories keep working.** Watch state is purged for the whole subtree so a recreated tree is watched again rather than silently ignored.

## Platform behaviour

|                    | Linux/macOS                | Windows                            |
| ------------------ | -------------------------- | ---------------------------------- |
| Shell              | `sh -c`                    | `cmd /S /C`                        |
| Process grouping   | `Setpgid`                  | `CREATE_NEW_PROCESS_GROUP`         |
| Graceful stop      | `SIGTERM` to process group | `CTRL_BREAK` to process group      |
| Forced stop        | `SIGKILL` to process group | `taskkill /T /F`                   |

On Windows the graceful step sends `CTRL_BREAK` rather than a plain `taskkill`. A bare `taskkill` posts a window-close message that a console application never acts on, so it would do nothing except burn the entire grace period before the forced kill. `CTRL_BREAK` reaches the whole process group and arrives in a Go server as `os.Interrupt`, so graceful-shutdown code actually runs. You will see a `^C` echo in the console when this happens; that is the console reporting the event, not an error.

## Development

| Task | Linux/macOS | Windows |
| --- | --- | --- |
| Build | `make build` | `.\make.ps1 build` |
| Run all tests | `make test` | `.\make.ps1 test` |
| Fast tests only | `make test-short` | `.\make.ps1 test-short` |
| End-to-end tests | `make test-e2e` | `.\make.ps1 test-e2e` |
| Race detector | `make test-race` | `.\make.ps1 test-race` |
| Coverage report | `make test-coverage` | `.\make.ps1 test-coverage` |
| Format | `make fmt` | `.\make.ps1 fmt` |
| Pre-commit sweep | `make check` | `.\make.ps1 check` |

Run `make help` or `.\make.ps1 help` for the full list.

The race detector needs cgo and a 64-bit C toolchain. On Windows that often is not present, so use WSL or rely on CI.

### Tests

- Unit tests cover the debouncer, the filter, the crash detector, the config loader, the process manager, and the controller. The controller is driven through fakes so the ordering invariants — serial builds, supersede-on-change, failed-build safety, crash restart — are asserted directly.
- Integration tests drive the watcher against a real temporary directory and real fsnotify events.
- End-to-end tests in [`e2e/`](./e2e) run a real `go build`, start a real server process, save a real file, and assert the reload happened. They need `go` on `PATH` and are skipped under `-short`.

## Project structure

```text
cmd/hotreload/main.go        CLI: flags, config resolution, signal handling
internal/config              Defaults, TOML file, flag precedence, validation
internal/controller          Orchestration and the reload state machine
internal/watcher             Recursive fsnotify wrapper
internal/debounce            Event coalescing
internal/filter              Which paths matter
internal/crash               Sliding-window crash-loop detection
internal/process             Build and server process lifecycle
e2e                          End-to-end tests
testserver                   Demo target (separate module)
```

## License

[MIT](./LICENSE)
