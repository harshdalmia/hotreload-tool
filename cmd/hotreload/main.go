package main

import (
	"flag"
	"log/slog"
	"os"
)

func main() {
	root := flag.String("root", ".", "Directory to watch")
	build := flag.String("build", "", "Build command")
	execCmd := flag.String("exec", "", "Exec command")
	flag.Parse()

	if *build == "" || *execCmd == "" {
		slog.Error("both --build and --exec are required")
		os.Exit(1)
	}

	slog.Info("hotreload scaffold", "root", *root)
}
