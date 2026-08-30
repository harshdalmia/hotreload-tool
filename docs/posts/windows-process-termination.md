# A measurement that lied: fixing Windows process termination in a Go hot reloader

I spent an afternoon adding tests to a hot-reload CLI I'd written. One of the things I wanted to confirm was that stopping a server on Windows was fast, because the tool does it on every single file save. I measured it. It came back at 0.65 seconds. I wrote that down as "fine" and moved on.

The real number was five seconds, on every reload.

This is the story of why the measurement was wrong, what `taskkill` actually does, and what you're supposed to use instead. If you spawn child processes from Go on Windows, at least one of these will probably bite you.

## What the tool has to do

A hot reloader's job is unglamorous: watch files, rebuild, restart the server. The restart is the fiddly part. You need to kill the old process *and everything it spawned* — otherwise a child keeps holding port 8080 and the new server fails to bind — and you'd like to do it politely first, so the server can close connections and flush logs before it dies.

On Unix that's a well-worn path. Start the child in its own process group with `Setpgid`, send `SIGTERM` to the negated pid to hit the whole group, wait, then escalate to `SIGKILL`:

```go
syscall.Kill(-pid, syscall.SIGTERM)
// wait for the grace period...
syscall.Kill(-pid, syscall.SIGKILL)
```

Windows has no `SIGTERM`. So what do you do?

## The original implementation

What I'd written — and what a lot of code on the internet does — was to reach for `taskkill`:

```go
// Graceful attempt: /T means "and all children".
exec.Command("taskkill", "/PID", pidStr, "/T").Run()

select {
case <-waitCh:
    return // exited politely
case <-time.After(gracePeriod): // 5 seconds
}

// Force it.
exec.Command("taskkill", "/PID", pidStr, "/T", "/F").Run()
```

The reasoning is tidy: `taskkill` without `/F` is the polite version, `/F` is the hammer, `/T` walks the tree. Ask nicely, wait, then insist.

It doesn't work, and the test I wrote to check it agreed with me anyway.

## The measurement

The test started a long-running process, called `Stop()`, and asserted it finished promptly:

```go
srv := NewServer(sleepCmd(60))
srv.Start(ctx)

start := time.Now()
srv.Stop()
elapsed := time.Since(start)

if elapsed > 4*time.Second {
    t.Errorf("Stop() took too long (%v)", elapsed)
}
```

`sleepCmd(60)` on Windows was:

```go
`powershell -NoProfile -Command "Start-Sleep -Seconds 60"`
```

0.65 seconds. Comfortably under the threshold. Graceful termination works on Windows, apparently. Moving on.

## Why it lied

Some time later I was fixing something unrelated, and it turned out that command was never sleeping at all.

Here's how the process was being started:

```go
exec.Command("cmd", "/C", command)
```

Go's `os/exec` on Windows has to flatten your arguments into a single command-line string, because that's what `CreateProcess` takes. It does that using the quoting rules of the Microsoft C runtime — the ones that `argv`-parsing programs understand.

`cmd.exe` does not use those rules.

So `cmd.exe` received something along these lines:

```
cmd /C "powershell -NoProfile -Command \"Start-Sleep -Seconds 60\""
```

Those `\"` sequences mean "a literal quote" to a C program and nothing in particular to `cmd.exe`. PowerShell got handed mangled arguments, failed, and exited on its own within about half a second.

So my measurement was real. It just wasn't measuring what I thought. I had timed how long it takes to reap a process that had already died of unrelated causes. The graceful kill path was never exercised.

The fix is to bypass Go's argument assembly entirely and hand Windows the command line verbatim. `cmd /S /C "…"` tells `cmd.exe` to strip the outermost quote pair and treat the remainder literally:

```go
func shellCommand(command string) *exec.Cmd {
    cmd := exec.Command("cmd")
    cmd.SysProcAttr = &syscall.SysProcAttr{
        CmdLine: `cmd /S /C "` + command + `"`,
    }
    return cmd
}
```

If you pass user-supplied shell commands to `cmd.exe` from Go, you probably need this. A build command as ordinary as `go build -o "C:\Program Files\app.exe" .` is broken without it.

## The bug underneath the bug

With the quoting fixed, the process genuinely ran for 60 seconds — and three tests immediately started failing. Not by a little:

```
--- FAIL: TestRunBuild_CancelledMidFlight (5.87s)
--- FAIL: TestServer_StopReleasesResources (5.72s)
--- FAIL: TestServer_StartHonoursContext (5.01s)
```

Five seconds. Exactly the grace period. Every stop was falling through the polite attempt, waiting out the full timeout, and only dying to the forced kill.

`taskkill` without `/F` doesn't terminate a console application. It posts a close request to the process's *windows*. A console program has no message loop and no window to receive it, so nothing happens. The call succeeds, reports success, and accomplishes nothing.

Which means the original code had never once shut a server down gracefully. It waited five seconds and then force-killed it — on every reload, in a tool whose entire purpose is to feel instant. The polite step wasn't just ineffective; it was pure latency.

## What you're actually supposed to use

The Windows equivalent of "please shut down" for a console process is a console control event: [`GenerateConsoleCtrlEvent`](https://learn.microsoft.com/en-us/windows/console/generateconsolectrlevent). It sends `CTRL_C_EVENT` or `CTRL_BREAK_EVENT` to a **process group** that shares your console.

Two details make it fit neatly.

First, the child is already being created with `CREATE_NEW_PROCESS_GROUP`, which was there for unrelated reasons. A side effect is that the group id equals the child's pid, and children inherit the group — so one call reaches the whole tree, including grandchildren spawned by the `cmd.exe` wrapper.

Second, Go's runtime already understands the event. Per [golang/go#6948](https://github.com/golang/go/issues/6948), `CTRL_C_EVENT` and `CTRL_BREAK_EVENT` are both converted to SIGINT for use by `os/signal`. A Go server doing the usual thing:

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
```

receives it and runs its shutdown path. Which is the entire point of having a graceful step.

```go
func signalBreak(pid int) error {
    // Group 0 means every process sharing this console, including us.
    if pid <= 0 {
        return fmt.Errorf("refusing to signal process group %d", pid)
    }
    return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid))
}
```

That `pid <= 0` guard is not decoration. Passing a group of 0 broadcasts to every process attached to the console — which includes the tool doing the sending. A hot reloader that kills itself on every reload would be a memorable bug.

`CTRL_BREAK` rather than `CTRL_C`, incidentally, because Ctrl+Break is always delivered as a signal, whereas [an application can disable Ctrl+C handling](https://stackoverflow.com/questions/72258181/ctrl-c-event-vs-ctrl-break-event-vs-terminateprocess-on-windows) or consume it as ordinary input.

The escalation now looks like its Unix counterpart: control event, wait for the grace period, then `taskkill /T /F`, which does work and reliably walks the tree.

## The result

From the reload log, with debug logging on:

```
19:24:07.682 DEBUG stopping server pid=23112
19:24:07.701 DEBUG process stopped after CTRL_BREAK pid=23112
```

**19 milliseconds**, against five seconds before. And unlike before, the server actually got a chance to shut down properly on the way out.

## A wrinkle worth knowing

Fixing this surfaced one more thing. Build cancellation — when you save a file while a compile is still running — went flaky, occasionally taking the full five seconds again.

A console control event sent milliseconds after `CreateProcess` can be missed. The child hasn't finished attaching to the console or installing its handler yet, so the event lands nowhere and you wait out the timeout for nothing.

For a *server*, that's rare enough not to matter; it's been up for a while by the time you stop it. For a *build*, it's the common case: cancellation happens moments after the compiler starts.

But a compiler has nothing to flush and no connections to drain. A graceful signal buys you nothing there. So build cancellation skips the polite step entirely and goes straight to the forced kill:

```go
// A grace of zero or less skips the graceful step.
killGroup(cmd.Process.Pid, "build", 0, waitCh)
```

Different processes want different termination policies, which is obvious in hindsight and wasn't at the time.

## What I'd take from this

**A passing test is not evidence that the code under test ran.** Mine asserted an upper bound on elapsed time, and an upper bound is satisfied beautifully by a process that crashed before you tried to kill it. If I'd asserted something about the *mechanism* — that the graceful path was the one that succeeded — it would have failed on day one.

**Bugs hide behind other bugs.** The quoting problem and the termination problem were independent, in different files, and one perfectly concealed the other. Fixing the first is what made the second visible, and I only found the first while looking for something else entirely.

**"It succeeded" and "it did something" are different claims.** `taskkill /T` returned exit code 0 every time. The API was working exactly as documented. I was asking it to do something it doesn't do.

**Cross-platform means reading the platform's docs, not translating your assumptions.** I'd mapped `SIGTERM` onto the thing that looked most like it, and the thing that looked most like it was a window-close message. The actual analogue was in a different part of the API surface, under "console", where I hadn't thought to look.

---

*The tool is [hotreload](https://github.com/harshdalmia/hotreload-tool) — a cross-platform hot-reload CLI in Go. The Windows process handling described here lives in [`internal/process/proc_windows.go`](https://github.com/harshdalmia/hotreload-tool/blob/main/internal/process/proc_windows.go).*
