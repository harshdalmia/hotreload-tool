// Command hotreload watches a directory tree and rebuilds and restarts a
// server whenever the source changes.
//
// Configuration comes from three places, in increasing order of precedence:
// built-in defaults, a .hotreload.toml file, and command-line flags. A flag
// left at its default value never overrides the config file.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/harshdalmia/hotreload-tool/internal/config"
	"github.com/harshdalmia/hotreload-tool/internal/controller"
)

// Build information, injected at link time:
//
//	go build -ldflags "-X main.version=1.2.3 -X main.commit=abc1234 -X main.date=2026-08-30"
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "hotreload: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("hotreload", flag.ContinueOnError)
	fs.Usage = func() { usage(fs) }

	defaults := config.Default()

	var (
		root       = fs.String("root", defaults.Root, "Directory to watch recursively for changes")
		build      = fs.String("build", "", "Command used to build the project when a change is detected (omit for interpreted projects)")
		execCmd    = fs.String("exec", "", "Command used to run the server (required)")
		preCmd     = fs.String("pre-cmd", "", "Command run before each build, e.g. a code generator (failure aborts the reload)")
		postCmd    = fs.String("post-cmd", "", "Command run after the server starts (failure is reported but changes nothing)")
		debounceFl = fs.Duration("debounce", defaults.Debounce, "Quiet window after the last change before rebuilding")
		killDelay  = fs.Duration("kill-delay", defaults.KillDelay, "Time a process gets to exit gracefully before being force-killed")
		poll       = fs.Bool("poll", false, "Detect changes by scanning instead of filesystem notifications (Docker bind mounts, network shares, WSL)")
		pollEvery  = fs.Duration("poll-interval", defaults.PollInterval, "How often to rescan the tree in --poll mode")
		configPath = fs.String("config", "", "Path to a config file (default: ./"+config.FileName+" if present)")
		verbose    = fs.Bool("verbose", false, "Enable verbose/debug logging")
		showVer    = fs.Bool("version", false, "Print version information and exit")

		includeExt stringList
		excludeDir stringList
		includeDir stringList
	)
	fs.Var(&includeExt, "include-ext", "Only rebuild for these file extensions, comma-separated (default: every non-ignored file)")
	fs.Var(&excludeDir, "exclude-dir", "Additional directory names to ignore, comma-separated")
	fs.Var(&includeDir, "include-dir", "Directory names to watch even though they are ignored by default, comma-separated")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *showVer {
		fmt.Printf("hotreload %s (commit %s, built %s)\n", version, commit, date)
		return nil
	}

	// Precedence: defaults < config file < explicitly-set flags.
	cfg := defaults

	path, err := resolveConfigPath(*configPath)
	if err != nil {
		return err
	}
	if path != "" {
		if err := cfg.ApplyFile(path); err != nil {
			return err
		}
	}

	// flag.Visit only reports flags the user actually passed, so a default
	// value can never silently clobber a config-file setting.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "root":
			cfg.Root = *root
		case "build":
			cfg.Build = *build
		case "exec":
			cfg.Exec = *execCmd
		case "pre-cmd":
			cfg.PreCmd = *preCmd
		case "post-cmd":
			cfg.PostCmd = *postCmd
		case "debounce":
			cfg.Debounce = *debounceFl
		case "kill-delay":
			cfg.KillDelay = *killDelay
		case "poll":
			cfg.Poll = *poll
		case "poll-interval":
			cfg.PollInterval = *pollEvery
		case "include-ext":
			cfg.IncludeExt = includeExt
		case "exclude-dir":
			cfg.ExcludeDir = excludeDir
		case "include-dir":
			cfg.IncludeDir = includeDir
		case "verbose":
			cfg.Verbose = *verbose
		}
	})

	if err := cfg.Normalize(); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		fs.Usage()
		return err
	}

	setupLogger(cfg.Verbose)

	attrs := []any{
		"version", version,
		"root", cfg.Root,
		"build", cfg.Build,
		"exec", cfg.Exec,
		"debounce", cfg.Debounce,
		"kill_delay", cfg.KillDelay,
	}
	if path != "" {
		attrs = append(attrs, "config", path)
	}
	if len(cfg.IncludeExt) > 0 {
		attrs = append(attrs, "include_ext", strings.Join(cfg.IncludeExt, ","))
	}
	if len(cfg.ExcludeDir) > 0 {
		attrs = append(attrs, "exclude_dir", strings.Join(cfg.ExcludeDir, ","))
	}
	if len(cfg.IncludeDir) > 0 {
		attrs = append(attrs, "include_dir", strings.Join(cfg.IncludeDir, ","))
	}
	if cfg.PreCmd != "" {
		attrs = append(attrs, "pre_cmd", cfg.PreCmd)
	}
	if cfg.PostCmd != "" {
		attrs = append(attrs, "post_cmd", cfg.PostCmd)
	}
	if cfg.Poll {
		attrs = append(attrs, "poll", true, "poll_interval", cfg.PollInterval)
	}
	slog.Info("hotreload starting", attrs...)

	// Top-level context — cancelled on SIGINT / SIGTERM.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	return controller.New(cfg).Run(ctx)
}

// resolveConfigPath returns the config file to load, or "" when there is none.
// An explicit --config that does not exist is an error; an absent default
// config file is not.
func resolveConfigPath(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("config file '%s': %w", explicit, err)
		}
		return explicit, nil
	}

	// Without a working directory there is nowhere to look for a default
	// config file. That is not fatal: running without one is the normal case,
	// and --config remains available.
	cwd, err := os.Getwd()
	if err != nil {
		//nolint:nilerr // deliberate: absence of a discoverable config is not an error
		return "", nil
	}
	if found, ok := config.FindFile(cwd); ok {
		return found, nil
	}
	return "", nil
}

func setupLogger(verbose bool) {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
}

func usage(fs *flag.FlagSet) {
	out := fs.Output()
	fmt.Fprintf(out, `hotreload %s - rebuild and restart your server on file change

Usage:
  hotreload --exec "<cmd>" [--build "<cmd>"] [flags]

Flags:
`, version)
	fs.PrintDefaults()
	fmt.Fprintf(out, `
Configuration file:
  If ./%[1]s exists it is loaded automatically. Flags you pass
  explicitly override it. Durations use Go syntax ("150ms", "2s").

    root          = "."
    build         = "go build -o ./bin/app ./cmd/app"
    exec          = "./bin/app"
    pre_cmd       = "templ generate"
    post_cmd      = ""
    debounce      = "150ms"
    kill_delay    = "5s"
    include_ext   = [".go", ".html"]
    exclude_dir   = ["testdata"]
    include_dir   = []
    poll          = false
    poll_interval = "500ms"
    verbose       = false

Examples:
  # Watch the current tree, rebuild and restart on any change
  hotreload --build "go build -o ./bin/app ./cmd/app" --exec "./bin/app"

  # Only recompile when Go or template files change
  hotreload --build "make build" --exec "./bin/app" --include-ext .go,.tmpl

  # Interpreted project: no build step, just restart
  hotreload --exec "python app.py" --include-ext .py

  # Same, but let a syntax check stand in for the build so a broken save
  # leaves the running server alone
  hotreload --build "python -m compileall -q ." --exec "python app.py" --include-ext .py

  # Run a code generator before every build
  hotreload --pre-cmd "templ generate" --build "go build -o ./bin/app ./cmd/app" --exec "./bin/app"

  # Inside Docker, or on a network share, where notifications never arrive
  hotreload --poll --build "go build -o ./bin/app ./cmd/app" --exec "./bin/app"

  # Sources live in a directory hotreload ignores by default
  hotreload --build "make" --exec "./out/app" --include-dir build
`, config.FileName)
}

// stringList is a flag.Value that accepts a comma-separated list and may be
// repeated. Both of these produce the same result:
//
//	--include-ext .go,.html
//	--include-ext .go --include-ext .html
type stringList []string

func (s *stringList) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(*s, ",")
}

func (s *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			*s = append(*s, trimmed)
		}
	}
	return nil
}

var _ flag.Value = (*stringList)(nil)
