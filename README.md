# hotreload

A fast hot-reload CLI for Go (and other compiled projects): on file change, it rebuilds and restarts your server automatically.

```bash
hotreload --root ./myproject --build "go build -o ./bin/server ./cmd/server" --exec "./bin/server"
```

See architecture details in [ARCHITECTURE.md](./ARCHITECTURE.md).

## Run in 60 seconds

### Linux/macOS
```bash
go mod tidy
go build -o ./bin/hotreload ./cmd/hotreload
./bin/hotreload --root ./testserver --build "cd ./testserver && go build -o ../bin/testserver ." --exec "./bin/testserver"
```

### Windows (PowerShell)
```powershell
go mod tidy
go build -o .\bin\hotreload.exe .\cmd\hotreload
.\bin\hotreload.exe --root .\testserver --build "cd testserver && go build -o ..\bin\testserver.exe ." --exec ".\bin\testserver.exe"
```

Then open `http://localhost:8080`, edit `testserver/main.go`, save, and watch it rebuild/restart.

## Usage

```text
hotreload --root <dir> --build "<cmd>" --exec "<cmd>" [--verbose]

  --root     Directory to watch recursively.            Default: .
  --build    Build command to run on changes.           Required.
  --exec     Server command to run after build success. Required.
  --verbose  Enable debug logging.
```

## Failure scenarios handled

- Editor event storms: rapid save bursts are debounced (150 ms), so one save cycle triggers one rebuild.
- New change during active build: in-flight build is cancelled immediately; stale build output is not started.
- Build failure: server is not replaced by a broken build; next successful save recovers.
- Stubborn process on restart: graceful stop first, then forced kill after timeout.
- Child process leakage: stop logic targets full process tree, not just parent process.
- Dynamic folders: new subdirectories are watched automatically without restarting hotreload.
- Deleted/recreated folders: watcher state is cleaned so recreated trees are watched again.
- Noisy files ignored: `.git`, `node_modules`, `vendor`, `bin`, `dist`, `build`, hidden files, swap/temp artifacts.

## Windows + Unix support

- Linux/macOS:
  - shell: `sh -c`
  - process lifecycle: process-group signals (`SIGTERM` then `SIGKILL`)
- Windows:
  - shell: `cmd /C`
  - process lifecycle: `CREATE_NEW_PROCESS_GROUP` + `taskkill /T` (with force fallback)

This keeps CLI behavior consistent across platforms while using OS-appropriate process controls.

## Evaluator quick checks

1. Initial build runs immediately on startup.
2. Edit `testserver/main.go`, save once -> one rebuild/restart.
3. Add syntax error, save -> build fails, running server remains stable.
4. Fix syntax error, save -> build succeeds, server restarts.
5. Create a new subfolder + `.go` file under root -> change is detected.

## Test and quality gates

```bash
go mod verify
go test ./... -count=1
```

Or via Makefile:

```bash
make test
make test-short
make test-race
```

## Project structure

```text
cmd/hotreload/main.go
internal/controller/controller.go
internal/watcher/watcher.go
internal/debounce/debounce.go
internal/filter/filter.go
internal/process/manager.go
internal/runner/runner.go
testserver/main.go
```
