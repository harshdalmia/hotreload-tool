// Package filter decides which paths should trigger a rebuild and which
// directories should be recursively watched.
//
// A Filter combines three configurable rules with a built-in set of noisy
// paths:
//
//   - ExcludeDir adds directory names to the ignore set.
//   - IncludeDir removes directory names from it, which is the only way to
//     watch a directory hotreload ignores by default (build, dist, tmp, or
//     any dot-directory).
//   - IncludeExt, when non-empty, restricts rebuild triggers to files with
//     those extensions. This is what stops a README edit from kicking off a
//     compile.
//
// Default() returns a Filter with the built-in rules and no overrides, and
// the package-level ShouldIgnoreDir / ShouldIgnoreFile functions delegate to
// it for callers that do not need configuration.
package filter

import (
	"path/filepath"
	"strings"
)

// defaultIgnoredDirs contains directory names that are never watched.
// Matched against every component of a path individually so that
// e.g. "src/.git/hooks" is correctly excluded.
var defaultIgnoredDirs = []string{
	// Version control
	".git", ".svn", ".hg",
	// JavaScript ecosystems
	"node_modules", ".npm", ".yarn", ".pnpm-store",
	// Go / general build output
	"vendor", "bin", "dist", "build",
	// Rust and Maven build output. Worth calling out: cargo rewrites
	// fingerprint files under target/ on every build, so watching it turns one
	// save into an endless rebuild loop.
	"target",
	// .NET / C intermediate output
	"obj",
	// Python virtual environments. The dotted forms (.venv, .tox) are already
	// covered by the hidden-directory rule; these are the bare ones.
	"venv",
	// IDE / editor metadata
	".idea", ".vscode",
	// Language caches
	"__pycache__", ".cache",
	// Test and coverage output
	"coverage",
	// Temporary directories
	"tmp", "temp",
}

// defaultIgnoredExts covers temp/swap/binary files that never represent a
// meaningful source-code change.
var defaultIgnoredExts = []string{
	// Vim
	".swp", ".swx",
	// Generic temp/backup
	".tmp", ".temp", ".bak", ".orig",
	// Compiled artefacts
	".pyc", ".pyo", ".class",
	".o",     // C / Go object files
	".a",     // static libraries
	".so",    // shared libraries
	".dylib", // macOS shared libraries
	".test",  // Go test binaries (go test -c)
}

// Options configures a Filter. All fields are optional.
type Options struct {
	// IncludeExt restricts rebuild triggers to these file extensions when
	// non-empty. Entries must be lowercase and dot-prefixed (config.Normalize
	// guarantees this).
	IncludeExt []string

	// ExcludeDir lists additional directory names to ignore.
	ExcludeDir []string

	// IncludeDir lists directory names to un-ignore. Takes precedence over
	// both the built-in ignore set and the all-dot-directories rule.
	IncludeDir []string
}

// Filter answers ignore questions for a single hotreload run.
// It is immutable after construction and safe for concurrent use.
type Filter struct {
	ignoredDirs map[string]bool
	ignoredExts map[string]bool
	includeExts map[string]bool // empty means "allow every extension"
	allowedDirs map[string]bool // IncludeDir overrides

	// root, when set, scopes every decision to the watched tree. See WithRoot.
	root string
}

// New builds a Filter from opts.
func New(opts Options) *Filter {
	f := &Filter{
		ignoredDirs: make(map[string]bool, len(defaultIgnoredDirs)+len(opts.ExcludeDir)),
		ignoredExts: make(map[string]bool, len(defaultIgnoredExts)),
		includeExts: make(map[string]bool, len(opts.IncludeExt)),
		allowedDirs: make(map[string]bool, len(opts.IncludeDir)),
	}
	for _, d := range defaultIgnoredDirs {
		f.ignoredDirs[d] = true
	}
	for _, e := range defaultIgnoredExts {
		f.ignoredExts[e] = true
	}
	for _, d := range opts.ExcludeDir {
		f.ignoredDirs[d] = true
	}
	for _, d := range opts.IncludeDir {
		f.allowedDirs[d] = true
		// An explicit include always wins over the built-in ignore set.
		delete(f.ignoredDirs, d)
	}
	for _, e := range opts.IncludeExt {
		f.includeExts[e] = true
		// Never let an include-ext entry be cancelled by the artefact list;
		// if the user says .so files matter, they matter.
		delete(f.ignoredExts, e)
	}
	return f
}

// Default returns a Filter with the built-in rules and no overrides.
func Default() *Filter {
	return New(Options{})
}

// WithRoot returns a copy of f that evaluates paths relative to root.
//
// This matters more than it looks. Directory names are matched per path
// component, so without a root the check runs over the whole absolute path and
// picks up directories the user never asked about. On Linux a project at
// /tmp/myproject is inside a component named "tmp", which is on the built-in
// ignore list, so every file in it was ignored and no reload ever fired. The
// same applied to anyone whose checkout lived under a directory called build,
// dist, temp, or vendor.
//
// Only what lies beneath the watched root is the user's project, so only that
// part is judged. Paths outside the root fall back to being evaluated whole.
//
// The returned Filter shares the receiver's rule maps, which are never
// mutated after construction.
func (f *Filter) WithRoot(root string) *Filter {
	scoped := *f
	scoped.root = root
	return &scoped
}

// scope reduces path to its portion below the root, so components above the
// root cannot trigger ignore rules.
func (f *Filter) scope(path string) string {
	if f.root == "" {
		return path
	}
	rel, err := filepath.Rel(f.root, path)
	if err != nil {
		// Mixed absolute/relative, or different volumes on Windows. Fall back
		// to judging the path as given rather than guessing.
		return path
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// Outside the watched tree.
		return path
	}
	return rel
}

// defaultFilter backs the package-level convenience functions.
var defaultFilter = Default()

// ShouldIgnoreDir reports whether path belongs to a directory that should be
// excluded from watching, using the default rules.
func ShouldIgnoreDir(path string) bool { return defaultFilter.ShouldIgnoreDir(path) }

// ShouldIgnoreFile reports whether path should never trigger a rebuild,
// using the default rules.
func ShouldIgnoreFile(path string) bool { return defaultFilter.ShouldIgnoreFile(path) }

// ShouldIgnoreDir reports whether path (or any component of it) belongs to a
// directory that should be excluded from file watching.
func (f *Filter) ShouldIgnoreDir(path string) bool {
	normalized := filepath.ToSlash(filepath.Clean(f.scope(path)))
	for _, part := range strings.Split(normalized, "/") {
		if part == "" || part == "." || part == ".." {
			continue
		}
		// An explicit --include-dir beats every ignore rule below.
		if f.allowedDirs[part] {
			continue
		}
		if f.ignoredDirs[part] {
			return true
		}
		// All hidden directories (dot-prefixed names not already listed).
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

// ShouldIgnoreFile reports whether path represents a file that should never
// trigger a rebuild: temp files, swap files, binary artefacts, hidden files,
// anything inside an ignored directory, or — when IncludeExt is configured —
// anything whose extension is not on the include list.
func (f *Filter) ShouldIgnoreFile(path string) bool {
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

	ext := strings.ToLower(filepath.Ext(base))

	// Known bad extensions.
	if f.ignoredExts[ext] {
		return true
	}
	// Opt-in extension allow-list. Checked after the artefact rules so a
	// stray .swp never sneaks through, and before the directory check so the
	// cheaper test runs first.
	if len(f.includeExts) > 0 && !f.includeExts[ext] {
		return true
	}
	// Propagate directory-level ignore to all files inside it.
	return f.ShouldIgnoreDir(filepath.Dir(path))
}

// IncludeExts returns the configured extension allow-list, or nil when every
// extension is allowed. Used for start-up logging.
func (f *Filter) IncludeExts() []string {
	if len(f.includeExts) == 0 {
		return nil
	}
	out := make([]string, 0, len(f.includeExts))
	for e := range f.includeExts {
		out = append(out, e)
	}
	return out
}
