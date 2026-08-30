// Package config holds the resolved runtime configuration for hotreload and
// the logic for building it from defaults, an optional TOML file, and CLI
// flags.
//
// Precedence, lowest to highest:
//
//	built-in defaults  <  config file  <  explicitly-set CLI flags
//
// "Explicitly set" is determined with flag.Visit, so a flag left at its
// default value never silently overrides a config-file setting.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// FileName is the config file hotreload looks for when --config is not given.
const FileName = ".hotreload.toml"

// Built-in defaults. Exported so --help text and tests can reference them.
const (
	DefaultDebounce  = 150 * time.Millisecond
	DefaultKillDelay = 5 * time.Second
)

// Config is the fully-resolved configuration used by the controller.
type Config struct {
	// Root is the directory watched recursively. Always an absolute path
	// after Normalize.
	Root string

	// Build is the shell command run when a change is detected. Empty means
	// there is nothing to compile, so a change restarts the server directly.
	Build string

	// Exec is the shell command that runs the built artefact.
	Exec string

	// Debounce is the quiet window after the last file event before a
	// rebuild is triggered.
	Debounce time.Duration

	// KillDelay is how long a process gets to exit gracefully before it is
	// force-killed.
	KillDelay time.Duration

	// IncludeExt, when non-empty, restricts rebuild triggers to files with
	// these extensions. Empty means every non-ignored file triggers a
	// rebuild. Entries are normalised to lowercase with a leading dot.
	IncludeExt []string

	// ExcludeDir adds directory names to the built-in ignore set.
	ExcludeDir []string

	// IncludeDir removes directory names from the built-in ignore set. This
	// is the escape hatch for projects whose sources live in a directory
	// hotreload ignores by default (build, dist, tmp, or any dot-directory).
	IncludeDir []string

	// Verbose switches the logger to debug level.
	Verbose bool
}

// Default returns the built-in configuration.
func Default() Config {
	return Config{
		Root:      ".",
		Debounce:  DefaultDebounce,
		KillDelay: DefaultKillDelay,
	}
}

// fileConfig mirrors Config for TOML decoding. Every scalar is a pointer so
// that "absent" is distinguishable from "set to the zero value".
type fileConfig struct {
	Root       *string  `toml:"root"`
	Build      *string  `toml:"build"`
	Exec       *string  `toml:"exec"`
	Debounce   *string  `toml:"debounce"`
	KillDelay  *string  `toml:"kill_delay"`
	IncludeExt []string `toml:"include_ext"`
	ExcludeDir []string `toml:"exclude_dir"`
	IncludeDir []string `toml:"include_dir"`
	Verbose    *bool    `toml:"verbose"`
}

// FindFile reports the path of the config file to use when the user did not
// pass --config. It looks for FileName in dir and returns ok=false if there
// is no such file.
func FindFile(dir string) (string, bool) {
	path := filepath.Join(dir, FileName)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", false
	}
	return path, true
}

// ApplyFile decodes the TOML file at path over c. Only keys present in the
// file are applied. Unknown keys are rejected so typos surface immediately
// rather than being silently ignored.
func (c *Config) ApplyFile(path string) error {
	var fc fileConfig
	md, err := toml.DecodeFile(path, &fc)
	if err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return fmt.Errorf("config %s: unknown key(s): %s", path, strings.Join(keys, ", "))
	}

	if fc.Root != nil {
		c.Root = *fc.Root
	}
	if fc.Build != nil {
		c.Build = *fc.Build
	}
	if fc.Exec != nil {
		c.Exec = *fc.Exec
	}
	if fc.Debounce != nil {
		d, err := time.ParseDuration(*fc.Debounce)
		if err != nil {
			return fmt.Errorf("config %s: debounce: %w", path, err)
		}
		c.Debounce = d
	}
	if fc.KillDelay != nil {
		d, err := time.ParseDuration(*fc.KillDelay)
		if err != nil {
			return fmt.Errorf("config %s: kill_delay: %w", path, err)
		}
		c.KillDelay = d
	}
	if fc.IncludeExt != nil {
		c.IncludeExt = fc.IncludeExt
	}
	if fc.ExcludeDir != nil {
		c.ExcludeDir = fc.ExcludeDir
	}
	if fc.IncludeDir != nil {
		c.IncludeDir = fc.IncludeDir
	}
	if fc.Verbose != nil {
		c.Verbose = *fc.Verbose
	}
	return nil
}

// Normalize resolves Root to an absolute path and canonicalises the
// extension and directory lists. Safe to call more than once.
func (c *Config) Normalize() error {
	abs, err := filepath.Abs(c.Root)
	if err != nil {
		// Single quotes rather than %q: on Windows, %q escapes every backslash
		// and turns C:\project into "C:\\project" in the error the user reads.
		return fmt.Errorf("resolve root '%s': %w", c.Root, err)
	}
	c.Root = abs

	c.IncludeExt = normalizeExts(c.IncludeExt)
	c.ExcludeDir = normalizeNames(c.ExcludeDir)
	c.IncludeDir = normalizeNames(c.IncludeDir)
	return nil
}

// Validate reports whether the configuration is usable.
//
// Build is deliberately not required. Interpreted projects have nothing to
// compile, and demanding a placeholder command from them would be noise.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Exec) == "" {
		return fmt.Errorf("an exec command is required (--exec or exec= in %s)", FileName)
	}
	if c.Debounce < 0 {
		return fmt.Errorf("debounce must not be negative, got %s", c.Debounce)
	}
	if c.KillDelay <= 0 {
		return fmt.Errorf("kill-delay must be positive, got %s", c.KillDelay)
	}
	info, err := os.Stat(c.Root)
	if err != nil {
		return fmt.Errorf("root '%s' is not readable: %w", c.Root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("root '%s' is not a directory", c.Root)
	}
	return nil
}

// normalizeExts lowercases each extension and ensures a single leading dot,
// so "GO", "go" and ".go" all become ".go". Empty entries are dropped.
func normalizeExts(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		e := strings.ToLower(strings.TrimSpace(raw))
		if e == "" || e == "." {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		if seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeNames trims each directory name and drops empties and duplicates.
// Names are compared case-sensitively because path components are.
func normalizeNames(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		n := strings.TrimSpace(raw)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
