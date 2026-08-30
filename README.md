# hotreload

[![CI](https://github.com/harshdalmia/hotreload-tool/actions/workflows/ci.yml/badge.svg)](https://github.com/harshdalmia/hotreload-tool/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/harshdalmia/hotreload-tool)](https://goreportcard.com/report/github.com/harshdalmia/hotreload-tool)
[![Release](https://img.shields.io/github/v/release/harshdalmia/hotreload-tool?sort=semver)](https://github.com/harshdalmia/hotreload-tool/releases)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](./LICENSE)

**A cross-platform hot-reload CLI in Go.** Rebuilds and restarts your server on file change — and keeps working inside Docker, on network shares, and on Windows, where notification-based reloaders quietly stop reloading.

```bash
hotreload --build "go build -o ./bin/server ./cmd/server" --exec "./bin/server"
```

<!--
  DEMO GIF SLOT — record and uncomment.

  Ideal 30-second clip: start hotreload, edit a file, watch it reload, then
  introduce a syntax error and show the old server still serving.

  On Windows: ScreenToGif. On Linux/WSL: vhs (github.com/charmbracelet/vhs).
  Save to docs/demo.gif, then uncomment the line below.

![hotreload reloading a Go server on save](docs/demo.gif)
-->

Works for any language whose project you can build and run with a shell command: Go, Rust, TypeScript, Java, and — with no build step at all — Python and Node.

---

## Engineering notes

The interesting parts of this project are the failure modes, not the feature list. Each of the following was found, measured, and fixed; the reasoning behind them lives in [ARCHITECTURE.md](./ARCHITECTURE.md).

### Every restart looked like a crash

Crash detection asked the server for its uptime *after* calling `Stop()`. Stop kills the process, whose reaper records the exit time, so the check reported "this process exited" on every single rebuild. Saving twice in quick succession was logged as a crash loop and inserted a real backoff before the server came up.

The fix was to make exits attributable: `Stop()` marks the exit as intentional before signalling anything, and only unintentional exits are published on the channel the controller watches. A restart can no longer be mistaken for a crash, which is also what made genuine restart-on-crash possible.

### Windows was paying a 5-second penalty on every reload

The graceful stop used `taskkill /PID <pid> /T` without `/F`. That posts a window-close message — which a console application with no message loop never acts on — so every reload waited out the entire grace period before force-killing. Replaced with `CTRL_BREAK` addressed to the process group, which reaches the whole tree and arrives in a Go server as `os.Interrupt`, so graceful-shutdown code actually runs.

**Measured: 5s → 19ms per reload.**

Worth recording how this was missed: an early measurement said the stop path took 0.65s and looked fine. It was measuring a command that a *separate* bug had corrupted into exiting on its own. The number was real and the conclusion was wrong.

### Go's argument quoting doesn't match cmd.exe's

`os/exec` quotes arguments using C runtime rules. `cmd.exe` does not follow them, so any build command containing quotes — `go build -o "C:\Program Files\app.exe" .` — arrived with those quotes escaped into literal characters and a path that didn't exist. Fixed by setting the command line verbatim via `SysProcAttr.CmdLine` with `cmd /S /C`.

### A save mid-build used to cost you your server

When a file changed while a build was running, the build was cancelled — but a build that had *already finished* would stop the running server and then discover it was stale, declining to start a replacement. You were left with nothing serving until the next build completed, or indefinitely if that build failed.

Moving the staleness check ahead of the stop fixed it. Related: the server was being started under the per-build context, which the next keystroke cancels, so it now starts under the root context instead.

### CI caught a bug that two of three platforms hid

Directory ignore rules were matched against the whole absolute path. Every Linux temp directory lives under `/tmp`, and `tmp` was on the ignore list, so under CI every file was filtered out and no event ever fired. It passed on Windows (temp is `...\AppData\Local\Temp` — capital T, case-sensitive match) and on macOS (`/var/folders/...`, no `tmp` component).

This was not a test artefact. `hotreload --root /tmp/myproject` on Linux would silently never reload, as would any checkout under a directory named `build`, `dist`, or `vendor`. Rules are now scoped to the watched root — what sits above it isn't your project. The regression test constructs the offending path by hand so it reproduces on every OS, and was verified by reverting the fix and watching it fail.

### Making the orchestrator testable

The controller holds every non-obvious invariant — serial builds, latest-state-wins cancellation, failed-build safety, crash backoff — and originally had no tests, because it constructed its watcher, builder, and server directly. Extracting three small interfaces made those invariants assertable with fakes rather than inferred from watching a reload happen.

There are also end-to-end tests that run a real `go build`, start a real process, save a real file, and assert the reload — including that a broken build leaves the previous server serving.

---

## Why another one?

[`air`](https://github.com/air-verse/air) is the established Go reloader, it's excellent, and it has years of use behind it that this does not. If you're doing Go work on Linux, use it.

This one exists because three problems kept coming up and were interesting to solve properly:

**Environments where filesystem notifications don't arrive.** inotify [doesn't fire for host-side writes on Docker bind mounts](https://forums.docker.com/t/inotify-is-not-triggering-any-events-in-docker-when-we-mount-it-from-the-host/104570), and is unreliable on network shares and some WSL setups. The usual workaround is to relocate your source tree. `--poll` makes it a flag instead.

**Windows as a first-class target rather than an afterthought.** Correct `cmd.exe` command-line construction, and `CTRL_BREAK` to the process group so your shutdown handlers actually run — see the engineering notes above for what both cost before they were fixed.

**Language independence.** It runs two shell commands and supervises a process tree, so nothing about it is Go-specific, and the build step is optional for interpreted projects.

Deliberately *not* claimed: maturity, an ecosystem, or a user base.

---

## Install

```bash
go install github.com/harshdalmia/hotreload-tool/cmd/hotreload@latest
```

Or grab a binary from the [releases page](https://github.com/harshdalmia/hotreload-tool/releases), or build from source:

```bash
make build          # Linux/macOS
.\make.ps1 build    # Windows
```

## Try it in 60 seconds

```bash
make demo           # Linux/macOS
.\make.ps1 demo     # Windows
```

Open `http://localhost:8080`, edit the `version` constant in `testserver/main.go`, save, and watch the server restart with the new value. Then introduce a syntax error and watch the old server keep serving.

## Usage

```text
hotreload --exec "<cmd>" [--build "<cmd>"] [flags]

  --exec          Command that runs your server.                      Required.
  --build         Command run when a change is detected.              Optional.
  --pre-cmd       Command run before each build.                      Optional.
  --post-cmd      Command run after the server starts.                Optional.
  --root          Directory watched recursively.                      Default: .
  --debounce      Quiet window after the last change.                 Default: 150ms
  --kill-delay    Grace period before a process is force-killed.      Default: 5s
  --include-ext   Only rebuild for these extensions, comma-separated. Default: all
  --exclude-dir   Extra directory names to ignore, comma-separated.
  --include-dir   Directory names to watch despite being ignored.
  --poll          Detect changes by scanning instead of notifications.
  --poll-interval How often to rescan in --poll mode.                 Default: 500ms
  --config        Path to a config file.                              Default: ./.hotreload.toml
  --verbose       Debug logging.
  --version       Print version information and exit.
```

`--build` and `--exec` are passed to your shell (`sh -c` on Unix, `cmd /S /C` on Windows), so pipes, redirects, globs, and environment variables all work.

### Docker, network shares, WSL

inotify does not fire for writes made on the host side of a Docker bind mount, and is unreliable across some network filesystems and WSL configurations. A notification-based reloader starts cleanly there, reports the directories it is watching, and then never reloads — the worst kind of failure, because nothing looks wrong.

```bash
hotreload --poll --build "go build -o ./bin/app ./cmd/app" --exec "./bin/app"
```

Polling costs a walk per interval (500ms by default) and reload latency is bounded by it. In exchange it works everywhere, needs no per-directory watch handles so large trees cost nothing extra, and has no window between a directory being created and its watch being armed.

hotreload also falls back to polling automatically when notifications can't be set up at all — an exhausted inotify watch limit, for instance. It cannot detect the case where watches are accepted but events never arrive, so in Docker you have to ask for `--poll`.

### Interpreted projects

`--build` is optional. Python, Node and Ruby have nothing to compile, so a save restarts the process directly:

```bash
hotreload --exec "python app.py"  --include-ext .py
hotreload --exec "node server.js" --include-ext .js,.mjs
```

It's still worth putting a syntax check in the build slot, because a failed build leaves the running server untouched — which turns the build stage into a guard against half-finished edits:

```bash
hotreload --build "python -m compileall -q ." --exec "python app.py"  --include-ext .py
hotreload --build "node --check server.js"    --exec "node server.js" --include-ext .js
```

### Compiled projects other than Go

```bash
hotreload --build "cargo build"    --exec "./target/debug/server"    --include-ext .rs
hotreload --build "npx tsc"        --exec "node dist/server.js"      --include-ext .ts
hotreload --build "mvn -q package" --exec "java -jar target/app.jar" --include-ext .java
```

Build output directories are ignored by default (`target`, `obj`, `bin`, `dist`, `build`, `coverage`, `node_modules`, `vendor`, `venv`, `__pycache__`). This matters more than it sounds: build tools rewrite files in their output directory on every run, so watching one turns a single save into an endless rebuild loop.

### Only rebuild for the files that matter

By default any non-ignored change triggers a rebuild, documentation included. Narrow it:

```bash
hotreload --build "make build" --exec "./bin/app" --include-ext .go,.tmpl
```

### Code generators and post-start hooks

```bash
hotreload \
  --pre-cmd "templ generate" \
  --build   "go build -o ./bin/app ./cmd/app" \
  --exec    "./bin/app"
```

A failing `--pre-cmd` aborts the reload and leaves the running server alone, exactly like a failed build — if the generator didn't run, the build would only compile stale sources. `--post-cmd` runs after the server starts, for seeding a database or pinging a health check; a failure there is reported but changes nothing, because the server is already up.

### Watching a directory that is ignored by default

```bash
hotreload --build "make" --exec "./out/app" --include-dir build
```

## Configuration file

If `.hotreload.toml` exists in the working directory it is loaded automatically. Precedence runs defaults, then the config file, then flags you passed explicitly — a flag left at its default never overrides the file.

```toml
root          = "."
build         = "go build -o ./bin/app ./cmd/app"
exec          = "./bin/app"
pre_cmd       = "templ generate"
post_cmd      = ""
debounce      = "150ms"
kill_delay    = "5s"
include_ext   = [".go", ".tmpl"]
exclude_dir   = ["testdata"]
include_dir   = []
poll          = false
poll_interval = "500ms"
verbose       = false
```

See [.hotreload.example.toml](./.hotreload.example.toml) for the annotated version. Unknown keys are rejected rather than ignored, so a typo fails loudly instead of silently doing nothing.

## Behaviour worth knowing

**Event storms are collapsed.** A single editor save can emit write, chmod, and two renames; a formatter can touch a whole package. Everything inside the debounce window becomes one rebuild.

**A failed build never disturbs a running server.** Your server keeps serving the last binary that compiled. Fix the error, save, and it recovers.

**The newest save always wins.** A change arriving mid-build cancels that build immediately. If the build had already finished, it declines to swap in its now-stale binary and leaves the running server alone.

**A crashed server is restarted.** If your server panics or exits non-zero on its own, it comes back without waiting for a file change. Three crashes within ten seconds is treated as a loop and adds a short backoff so the stack trace stays readable; a successful rebuild clears that history.

**A clean exit is not restarted.** A server that exits with status 0 is assumed to have meant it, so a one-shot `--exec` doesn't spin forever.

**Whole process trees are stopped.** Your server's children go with it, so nothing is left holding the port. This matters for wrappers like `npm start` or `mvn exec`, where the real process is a child.

**Output is labelled.** Server lines are tagged `[app]`, the build's `[build]`, hooks `[pre]` and `[post]`, dimmed so they stay out of the way. Colour is disabled automatically when output isn't a terminal, and honours `NO_COLOR`.

**New directories are picked up while running.** Including ones that arrive already populated — a branch switch, an unzip, a `cp -r` — where the files were created before any watch existed.

**Deleted and recreated directories keep working.** Watch state is purged for the whole subtree, so a recreated tree is watched again rather than silently ignored.

## Platform behaviour

|                  | Linux/macOS                | Windows                       |
| ---------------- | -------------------------- | ----------------------------- |
| Shell            | `sh -c`                    | `cmd /S /C`                   |
| Process grouping | `Setpgid`                  | `CREATE_NEW_PROCESS_GROUP`    |
| Graceful stop    | `SIGTERM` to process group | `CTRL_BREAK` to process group |
| Forced stop      | `SIGKILL` to process group | `taskkill /T /F`              |

You will see a `^C` echo in the Windows console on each reload. That's the console reporting the `CTRL_BREAK` event, not an error.

## Known limitations

- **Symlinks are not followed.** A project with symlinked source directories won't be watched through the link, in either watcher.
- **Docker can't be auto-detected.** Nothing reports "watches accepted but events never delivered", so `--poll` has to be requested rather than inferred.
- **macOS pays more for large trees.** fsnotify uses kqueue there, which needs an open file descriptor per *file* rather than per directory. `--poll` avoids it entirely.
- **`--exec` must stay in the foreground.** A command that daemonises and returns looks like an immediate exit.
- **Not a substitute for HMR.** Vite, webpack-dev-server and friends do in-process module replacement and hold browser connections; restarting them is strictly worse.

## Development

| Task | Linux/macOS | Windows |
| --- | --- | --- |
| Build | `make build` | `.\make.ps1 build` |
| Run all tests | `make test` | `.\make.ps1 test` |
| Fast tests only | `make test-short` | `.\make.ps1 test-short` |
| End-to-end tests | `make test-e2e` | `.\make.ps1 test-e2e` |
| Race detector | `make test-race` | `.\make.ps1 test-race` |
| Coverage report | `make test-coverage` | `.\make.ps1 test-coverage` |
| Lint | `make lint` | `.\make.ps1 lint` |
| Pre-commit sweep | `make check` | `.\make.ps1 check` |

Run `make help` or `.\make.ps1 help` for the full list. The race detector needs cgo and a 64-bit C toolchain; on Windows use WSL or rely on CI.

CI runs a three-OS matrix (Linux, macOS, Windows) across two Go versions, plus separate jobs for the race detector, coverage, linting, and a release dry run. The platform-specific process and watcher code is the riskiest part of this project, so a single-OS pipeline wouldn't be worth much — as the `/tmp` bug above demonstrates.

### Tests

- **Unit tests** cover the debouncer, filter, crash detector, config loader, log prefixing, and process manager.
- **Controller tests** drive the reload state machine through fakes, asserting the ordering invariants directly: serial builds, supersede-on-change, failed-build safety, crash restart and backoff, hook ordering.
- **Integration tests** run both watchers against real temporary directories and real filesystem events.
- **End-to-end tests** in [`e2e/`](./e2e) run a real `go build`, start a real server, save a real file, and assert the reload — plus build-failure survival, crash restart, poll mode, hooks, and clean shutdown leaving no orphan process.

## Project structure

```text
cmd/hotreload/main.go   CLI: flags, config resolution, signal handling
internal/config         Defaults, TOML file, flag precedence, validation
internal/controller     Orchestration and the reload state machine
internal/watcher        fsnotify-backed watcher and the polling fallback
internal/debounce       Event coalescing
internal/filter         Which paths matter
internal/crash          Sliding-window crash-loop detection
internal/process        Build and server process lifecycle, signals, process trees
internal/logstream      Prefixed, colour-aware child process output
e2e                     End-to-end tests
testserver              Demo target (separate module)
```

## License

[MIT](./LICENSE)
