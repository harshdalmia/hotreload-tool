# ARCHITECTURE

## Goals

- Detect file changes quickly across a project tree.
- Rebuild and restart server automatically with minimal delay.
- Prefer latest saved state over intermediate stale states.
- Handle process shutdown robustly, including child processes.
- Stay stable during long-running development sessions.

## High-level flow

1. Watch filesystem recursively under `--root`.
2. Filter noisy/irrelevant paths.
3. On any meaningful event:
   - cancel in-flight build immediately
   - feed debouncer
4. After 150 ms quiet window, trigger one rebuild.
5. If build succeeds:
   - stop previous server process tree
   - skip restart if build context was cancelled (stale)
   - start new server process
6. Stream logs continuously from build and server processes.

## Component model

### `cmd/hotreload/main.go`

- Parses CLI flags.
- Configures `slog` logger.
- Wires root context cancellation on SIGINT/SIGTERM.
- Starts controller.

### `internal/controller`

- Orchestrates watcher, debouncer, build, and server lifecycle.
- Maintains `cancelInFlight` and `inFlightDone` to guarantee:
  - serial build execution
  - latest-state-wins semantics
- Triggers initial build immediately on startup.

### `internal/watcher`

- Wraps `fsnotify` with recursive watch behavior.
- Adds watches for newly created directories while running.
- On remove/rename, purges tracked path plus descendants to avoid stale watch state.
- Forwards filtered events to controller.

### `internal/debounce`

- Coalesces noisy event bursts into one rebuild trigger.
- Default quiet window: 150 ms.

### `internal/filter`

- Ignores directories/files that should not trigger rebuilds.
- Covers VCS metadata, dependencies, build outputs, hidden files, swap/temp artifacts.

### `internal/process`

- Runs build command and manages server process lifecycle.
- Streams stdout/stderr directly for real-time visibility.
- Stops process tree with graceful-then-force escalation and waits for reap.

### `internal/runner`

- Standalone build+run utility with crash-loop detection helpers.
- Keeps dedicated tests for restart-loop behavior.

## Key invariants

- At most one build goroutine is active at a time.
- A cancelled build must never start a server.
- A new restart must wait until previous server has fully exited.
- Filtered/ignored file events must not trigger rebuild.

## Concurrency design

- Watcher runs in its own goroutine and emits events.
- Controller event fan-in goroutine cancels builds immediately on event.
- Debouncer goroutine controls build-start timing.
- Controller main loop serializes stop/build/start transitions.

## Process management strategy

### Unix

- Commands run via `sh -c`.
- Process group semantics via `Setpgid`.
- Stop sequence: SIGTERM -> wait -> SIGKILL -> wait for `Wait()` completion.

### Windows

- Commands run via `cmd /C`.
- New process group via `CREATE_NEW_PROCESS_GROUP`.
- Stop sequence: `taskkill /PID <pid> /T` -> wait -> `taskkill /F` fallback -> wait.

## Error handling policy

- Build errors are logged and do not crash hotreload.
- Watch-add failures (e.g., watch limits) are warned and system continues.
- Server crashes are detected; crash-loop logic applies backoff.

## Performance notes

- Target responsiveness: typically within ~2 seconds including build time.
- Debounce collapses event storms to prevent redundant rebuilds.
- Buffered channels reduce backpressure and dropped-work risk.

## Test strategy

- Unit tests for debounce and filter behavior.
- Integration-style watcher tests for dynamic and recreated directories.
- Process tests for cancellation, stop idempotence, and stubborn process handling.
- Crash-loop tests in runner package.

## Tradeoffs

- Strong correctness preference over maximum aggressiveness:
  - stale builds are cancelled even if near completion.
- Platform-specific process control implementations add complexity, but ensure native reliability on both Unix and Windows.
- Default debounce is fixed in code today; future enhancement can expose `--debounce` for tuning.
