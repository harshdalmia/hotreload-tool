# ARCHITECTURE

## Goals

- Detect file changes quickly across a project tree.
- Rebuild and restart the server automatically with minimal delay.
- Prefer the latest saved state over any intermediate stale state.
- Never leave the developer without a working server because of a bad save.
- Handle process shutdown robustly, including child processes.
- Stay stable during long-running development sessions.

## High-level flow

1. Watch the filesystem recursively under `--root`.
2. Filter noisy and irrelevant paths.
3. On any meaningful event:
   - cancel the in-flight build immediately
   - feed the debouncer
4. After the quiet window (default 150 ms), trigger one rebuild.
5. Run `--pre-cmd` if set, then the build. A failing pre-command aborts the reload without touching the server, since the build would only compile what the generator failed to produce.
6. If the build succeeds:
   - bail out if a newer change arrived while compiling, leaving the current server untouched
   - otherwise stop the previous server process tree and start the new one
   - run `--post-cmd` if set; a failure here is reported but changes nothing, since the server is already up
7. Independently, if the server exits without being asked to, restart it.
8. Stream build and server output continuously, tagged by source.

## Component model

### `cmd/hotreload/main.go`

- Parses flags into a `config.Config`.
- Resolves configuration precedence: defaults, then `.hotreload.toml`, then explicitly-passed flags (detected with `flag.Visit`, so a default value never overrides the file).
- Configures the `slog` logger.
- Wires root context cancellation to SIGINT/SIGTERM.
- Starts the controller.

### `internal/config`

- Owns the `Config` type, the built-in defaults, and TOML decoding.
- Rejects unknown config keys so typos fail loudly.
- `Normalize` makes `Root` absolute and canonicalises extension and directory lists (`go`, `.go`, and `GO` all become `.go`).
- `Validate` checks the required commands and that `Root` is a readable directory.

### `internal/controller`

- Orchestrates the watcher, debouncer, builder, and server.
- Depends on the `Builder`, `Server`, and `Watcher` interfaces rather than concrete types, which is what makes the reload state machine unit-testable with fakes.
- Maintains `cancelInFlight` and `inFlightDone` to guarantee serial builds and latest-state-wins semantics.
- Watches `Server.Exits()` so a crash is handled without a file event.
- Triggers an initial build immediately on startup.

### `internal/logstream`

- Tags child process output so `[app]`, `[build]`, `[pre]` and `[post]` lines are distinguishable at a glance.
- Writers are line-buffered because processes write in arbitrary chunks and a prefix must only appear at the start of a line. A `Sink` owns the underlying stream and its lock, so two tagged streams cannot interleave mid-line.
- Colour is disabled unless the target is a character device, and honours `NO_COLOR` and `TERM=dumb`. On Windows it enables virtual terminal processing first: a console without it prints escape sequences literally, so the prefixes would arrive as visible junk.

### `internal/watcher`

- Two interchangeable implementations behind the controller's `Watcher` interface: `Watcher`, backed by fsnotify, and `Poller`, which rescans on an interval.
- Polling exists because notifications are not always available. They do not fire for writes on the host side of a Docker bind mount, nor reliably across some network filesystems and WSL setups, and there a notification-based watcher starts cleanly and then never reloads. It also avoids per-directory watch handles entirely, so large trees cost nothing extra, and has no window between a directory being created and its watch being armed.
- Polling fingerprints files on modification time and size. A content hash would also catch a write that preserves both, but it would mean reading every file on every tick, which is the cost polling exists to keep small.
- The first scan establishes a baseline and emits nothing, since the controller triggers an initial build of its own.

### `internal/watcher` (fsnotify path)

- Wraps `fsnotify` with recursive watch behaviour.
- Adds watches for newly created directories while running.
- When a new directory arrives already populated, emits one synthetic event: those files were created before the watch existed, so fsnotify will never report them.
- On remove/rename, purges the tracked path plus all descendants to avoid stale watch state.
- Blocks rather than dropping when forwarding events, so no change is silently discarded.
- Always watches its own root, even if the root's name is on the ignore list — the user asked for it explicitly.

### `internal/debounce`

- Coalesces event bursts into one rebuild trigger.
- Default quiet window: 150 ms, configurable via `--debounce`.

### `internal/filter`

- Decides which directories are watched and which files trigger rebuilds.
- Built-in ignores cover VCS metadata, dependency trees, build output, hidden files, and editor swap/temp artefacts. Build output is the load-bearing case: build tools rewrite files in their output directory on every run, so watching one turns a single save into an endless rebuild loop. `target` covers cargo and Maven, `obj` covers .NET, alongside `bin`, `dist`, `build` and `coverage`.
- Configurable through `--include-ext` (an allow-list of extensions), `--exclude-dir` (additions to the ignore set), and `--include-dir` (removals from it, including the otherwise inescapable all-dot-directories rule).
- Directory rules are matched per path component, and only against the portion of a path *below* the watched root. Scoping matters: an unscoped check runs over the whole absolute path, so a project at `/tmp/myproject` sits inside a component named `tmp` that the built-in rules ignore, and nothing under it is ever reported. Anything above the root is not the user's project and is not judged.

### `internal/crash`

- Sliding-window crash-loop detector.
- Records crash timestamps and reports a loop once the threshold is reached within the window (default: 3 crashes in 10 seconds), asking for a backoff.
- A sliding window rather than a consecutive-failure counter, so a server that limps along between crashes clears itself without needing an explicit success signal.

### `internal/process`

- Runs the build command and manages the server process lifecycle.
- Streams stdout/stderr directly for real-time visibility.
- Stops process trees with graceful-then-force escalation and waits for reap.
- Attributes every exit: an exit caused by `Stop` is marked `Intentional`, and only unintentional exits are published on `Exits()`.

## Key invariants

- At most one build goroutine is active at a time. `stopInFlight` blocks on the previous goroutine's done channel before a new one launches.
- A cancelled build never starts a server.
- A cancelled build never *stops* one either. The supersede check runs before `Stop`, so a build that lost the race leaves the live server alone instead of killing it and declining to provide a replacement.
- The server is started under the root context, never a per-build context. Build contexts are cancelled on the next file event; the server must outlive that.
- A new restart waits until the previous server has fully exited. When even a forced kill fails, the process handle is retained so a later `Stop` retries, and the failure is logged rather than silently swallowed.
- Exits that hotreload caused are never counted as crashes.
- Filtered and ignored file events never trigger a rebuild.

## Concurrency design

- The watcher runs in its own goroutine and emits filtered events.
- A fan-in goroutine cancels the in-flight build on every event and forwards paths to the debouncer.
- The debouncer goroutine controls build-start timing.
- The controller main loop serialises stop/build/start transitions and selects over four cases: shutdown, build-goroutine completion, unexpected server exit, and rebuild trigger.
- A `sync.Mutex` guards `cancelInFlight`, which is reachable from both the fan-in goroutine and the main loop. `inFlightDone` is deliberately unguarded and only touched by the main loop.
- `buildWatch` is nil whenever no build is running. A nil channel blocks forever in a `select`, which is what lets the "no build in flight" case fall through without special-casing.

### Why cancellation is separate from debouncing

The debounce window controls when the *next* build starts. Cancellation is immediate and unconditional on every event.

Without that split, a build finishing inside the debounce window would see an uncancelled context and start a server built from code that predates the most recent save. Cancelling immediately means the post-build check reliably detects that the binary is stale.

## Process management strategy

### Unix

- Commands run via `sh -c`.
- Process group semantics via `Setpgid`.
- Stop sequence: `SIGTERM` to the group, wait for the kill delay, `SIGKILL` to the group, wait for `Wait()` to complete.
- If the group signal fails, the single pid is signalled as a fallback.

### Windows

- Commands run via `cmd /S /C`, with the command line set explicitly rather than assembled from `Args`. `os/exec` quotes arguments using C runtime rules that `cmd.exe` does not follow, so a build command containing quoted paths would otherwise arrive with those quotes escaped and broken.
- New process group via `CREATE_NEW_PROCESS_GROUP`, which makes the group id equal the child's pid and lets the whole tree be addressed without touching hotreload itself.
- Stop sequence: `CTRL_BREAK` to the process group, wait for the kill delay, `taskkill /PID <pid> /T /F`, wait for reap.
- A plain `taskkill` without `/F` is deliberately *not* the graceful step. It posts a window-close message that a console application with no message loop never acts on, so it accomplishes nothing except consuming the entire grace period. `CTRL_BREAK` reaches the group and surfaces in a Go server as `os.Interrupt`.

## Error handling policy

- Build errors are logged; the running server is left alone and hotreload continues.
- An absent build command is not an error. Interpreted projects have nothing to compile, so `Builder.Build` treats an empty command as immediate success and the reload becomes a plain restart. The low-level `RunBuild` helper stays strict, so only the deliberate choice is tolerated.
- Watch-add failures (inotify limits, permissions) are warned about and the walk continues rather than aborting.
- A server that exits non-zero on its own is restarted, with crash-loop backoff.
- A server that exits zero on its own is reported but not restarted, so a one-shot command cannot spin.
- A server that cannot be killed is reported as an error and its handle retained for retry.
- A failed `Start` is logged and the main loop keeps running.

## Performance notes

- Debouncing collapses event storms into one rebuild.
- The event channel is buffered (256) and sends block on the consumer rather than dropping, so a burst is absorbed without losing changes.
- The debouncer's output channel has capacity 1 and drops rather than queues. That is coalescing, not lost work: the channel carries no data, so a second pending trigger would say nothing the first does not.
- Only one directory scan happens per newly created directory, and the search for a seed file stops at the first match rather than walking the whole subtree.

## Test strategy

- Unit tests for the debouncer, filter, crash detector, and config loader.
- Controller tests drive the reload state machine through `Builder`, `Server`, and `Watcher` fakes. They cover the initial build, burst coalescing, failed-build safety, the supersede path, server lifetime context, crash restart, clean-exit handling, crash-loop backoff, and shutdown.
- Process tests cover build cancellation, stop idempotence, exit attribution, context handling, and a stubborn process that ignores `SIGTERM`.
- Watcher tests run against real temporary directories and real fsnotify events, including the deleted-and-recreated subtree case and the pre-populated new directory case.
- End-to-end tests run a real `go build`, start a real server, and assert reload, build-failure survival, crash restart, ignored-file inertness, and clean shutdown.

## Tradeoffs

- Correctness is preferred over aggressiveness: stale builds are cancelled even when nearly complete.
- Platform-specific process control adds complexity but is the only way to get reliable tree termination on both Unix and Windows.
- Unexpected clean exits are not restarted. This makes hotreload less useful for a server that legitimately exits zero and expects to be revived, in exchange for not turning a misconfigured `--exec` into an infinite loop.
- Crash-loop backoff blocks the main loop for its duration, so a file change during the backoff is handled after it rather than interrupting it. The window is short and the alternative is a materially more complex state machine.
- `include_ext` defaults to allowing everything. Defaulting to `.go` would suit Go projects but would silently break the Rust, C, and TypeScript users the tool otherwise supports.
