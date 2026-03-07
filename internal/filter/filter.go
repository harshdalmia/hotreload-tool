package filter

import (
	"path/filepath"
	"strings"
)

var ignoredDirs = map[string]bool{

	".git": true,
	".svn": true,
	".hg":  true,

	"node_modules": true,
	".npm":         true,
	".yarn":        true,
	".pnpm-store":  true,

	"vendor": true,
	"bin":    true,
	"dist":   true,
	"build":  true,

	".idea":   true,
	".vscode": true,

	"__pycache__": true,
	".cache":      true,

	"tmp":  true,
	"temp": true,
}

var ignoredExtensions = map[string]bool{

	".swp": true,
	".swx": true,

	".tmp":  true,
	".temp": true,
	".bak":  true,
	".orig": true,

	".pyc":   true,
	".pyo":   true,
	".class": true,
	".o":     true,
	".a":     true,
	".so":    true,
	".dylib": true,
	".test":  true,
}

func ShouldIgnoreDir(path string) bool {
	return shouldIgnoreDir(path)
}

func ShouldIgnoreFile(path string) bool {
	return shouldIgnoreFile(path)
}

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

		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

func shouldIgnoreFile(path string) bool {
	base := filepath.Base(path)

	if strings.HasPrefix(base, ".#") {
		return true
	}

	if strings.HasPrefix(base, ".") {
		return true
	}

	if strings.HasSuffix(base, "~") {
		return true
	}

	if strings.HasPrefix(base, "#") && strings.HasSuffix(base, "#") {
		return true
	}

	if ignoredExtensions[filepath.Ext(base)] {
		return true
	}

	if shouldIgnoreDir(filepath.Dir(path)) {
		return true
	}
	return false
}
