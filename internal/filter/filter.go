// Package filter decides which paths should trigger a rebuild and which
// directories should be recursively watched.
//
// Two functions are exported for use by the watcher:
//   - ShouldIgnoreDir  — returns true for directories that must never be watched
//   - ShouldIgnoreFile — returns true for files that must never trigger a rebuild
//
// The package-level helpers (shouldIgnoreDir / shouldIgnoreFile) contain the
// actual logic and are white-box tested within the same package.
package filter

import (
	"path/filepath"
	"strings"
)

// ignoredDirs contains directory names that should never be watched.
// Matched against every component of the path individually so that
// e.g. "src/.git/hooks" is correctly excluded.
var ignoredDirs = map[string]bool{
	// Version control
	".git": true,
	".svn": true,
	".hg":  true,
	// JavaScript ecosystems
	"node_modules": true,
	".npm":         true,
	".yarn":        true,
	".pnpm-store":  true,
	// Go / general build output
	"vendor": true,
	"bin":    true, // common binary output dir (go build -o ./bin/…)
	"dist":   true, // common distribution dir
	"build":  true, // cmake / generic build output
	// IDE / editor metadata
	".idea":    true,
	".vscode":  true,
	// Language caches
	"__pycache__": true,
	".cache":      true,
	// Temporary directories
	"tmp":  true,
	"temp": true,
}

// ignoredExtensions covers temp/swap/binary files that never represent a
// meaningful source-code change.
var ignoredExtensions = map[string]bool{
	// Vim
	".swp": true,
	".swx": true,
	// Generic temp/backup
	".tmp":  true,
	".temp": true,
	".bak":  true,
	".orig": true,
	// Compiled artefacts
	".pyc":  true,
	".pyo":  true,
	".class": true,
	".o":    true,  // C / Go object files
	".a":    true,  // static libraries
	".so":   true,  // shared libraries
	".dylib": true, // macOS shared libraries
	".test": true,  // Go test binaries (go test -c)
}

// ShouldIgnoreDir returns true when path (or any component of it) belongs
// to a directory that should be excluded from file watching.
func ShouldIgnoreDir(path string) bool {
	return shouldIgnoreDir(path)
}

// ShouldIgnoreFile returns true when path represents a file that should never
// trigger a rebuild (temp files, swap files, binary artefacts, hidden files, …).
func ShouldIgnoreFile(path string) bool {
	return shouldIgnoreFile(path)
}

// shouldIgnoreDir is the internal implementation, tested directly by the
// white-box tests in filter_test.go (same package).
func shouldIgnoreDir(path string) bool {
	normalized := filepath.ToSlash(filepath.Clean(path))
	parts := strings.Split(normalized, "/")
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if ignoredDirs[part] {
			return true
		}
		// All hidden directories (dot-prefixed names not already listed).
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

// shouldIgnoreFile is the internal implementation, tested directly.
func shouldIgnoreFile(path string) bool {
	base := filepath.Base(path)

	// Emacs lock file: .#filename
	if strings.HasPrefix(base, ".#") {
		return true
	}
	// Any other hidden file
	if strings.HasPrefix(base, ".") {
		return true
	}
	// Backup files: filename~
	if strings.HasSuffix(base, "~") {
		return true
	}
	// Emacs auto-save: #filename#
	if strings.HasPrefix(base, "#") && strings.HasSuffix(base, "#") {
		return true
	}
	// Known bad extensions
	if ignoredExtensions[filepath.Ext(base)] {
		return true
	}
	// Propagate directory-level ignore to all files inside it.
	if shouldIgnoreDir(filepath.Dir(path)) {
		return true
	}
	return false
}
