// Command hotreload watches a project directory for file changes and
// automatically rebuilds and restarts a server when changes are detected.
//
// Usage:
//
//	hotreload --root ./myproject --build "go build -o ./bin/server ./cmd/server" --exec "./bin/server"
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/example/hotreload/internal/controller"
)

func main() {
	root := flag.String("root", ".", "Directory to watch for file changes (including all subfolders)")
	build := flag.String("build", "", "Command used to build the project when a change is detected (required)")
	exec := flag.String("exec", "", "Command used to run the built server after a successful build (required)")
	verbose := flag.Bool("verbose", false, "Enable verbose/debug logging")
	flag.Parse()

	if *build == "" || *exec == "" {
		slog.Error("both --build and --exec flags are required")
		flag.Usage()
		os.Exit(1)
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	rootAbs, err := filepath.Abs(*root)
	if err != nil {
		slog.Error("failed to resolve root directory", "path", *root, "err", err)
		os.Exit(1)
	}

	slog.Info("hotreload starting",
		"root", rootAbs,
		"build", *build,
		"exec", *exec,
	)

	// Top-level context — cancelled on SIGINT / SIGTERM.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	c, err := controller.New(rootAbs, *build, *exec)
	if err != nil {
		slog.Error("failed to initialise controller", "err", err)
		os.Exit(1)
	}

	if err := c.Run(ctx); err != nil {
		slog.Error("hotreload stopped with error", "err", err)
		os.Exit(1)
	}
}
