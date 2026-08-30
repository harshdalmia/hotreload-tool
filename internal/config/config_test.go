package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
	return path
}

func TestDefault(t *testing.T) {
	c := Default()
	if c.Root != "." {
		t.Errorf("Root = %q, want %q", c.Root, ".")
	}
	if c.Debounce != DefaultDebounce {
		t.Errorf("Debounce = %v, want %v", c.Debounce, DefaultDebounce)
	}
	if c.KillDelay != DefaultKillDelay {
		t.Errorf("KillDelay = %v, want %v", c.KillDelay, DefaultKillDelay)
	}
	if c.Build != "" || c.Exec != "" {
		t.Error("Build and Exec should have no default; they are required")
	}
}

// --- ApplyFile --------------------------------------------------------------

func TestApplyFile_AllKeys(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, FileName, `
root        = "./src"
build       = "go build -o ./bin/app ./cmd/app"
exec        = "./bin/app"
debounce    = "250ms"
kill_delay  = "3s"
include_ext = [".go", ".tmpl"]
exclude_dir = ["testdata"]
include_dir = ["build"]
verbose     = true
`)

	c := Default()
	if err := c.ApplyFile(path); err != nil {
		t.Fatalf("ApplyFile: %v", err)
	}

	if c.Root != "./src" {
		t.Errorf("Root = %q, want %q", c.Root, "./src")
	}
	if c.Build != "go build -o ./bin/app ./cmd/app" {
		t.Errorf("Build = %q", c.Build)
	}
	if c.Exec != "./bin/app" {
		t.Errorf("Exec = %q", c.Exec)
	}
	if c.Debounce != 250*time.Millisecond {
		t.Errorf("Debounce = %v, want 250ms", c.Debounce)
	}
	if c.KillDelay != 3*time.Second {
		t.Errorf("KillDelay = %v, want 3s", c.KillDelay)
	}
	if strings.Join(c.IncludeExt, ",") != ".go,.tmpl" {
		t.Errorf("IncludeExt = %v", c.IncludeExt)
	}
	if strings.Join(c.ExcludeDir, ",") != "testdata" {
		t.Errorf("ExcludeDir = %v", c.ExcludeDir)
	}
	if strings.Join(c.IncludeDir, ",") != "build" {
		t.Errorf("IncludeDir = %v", c.IncludeDir)
	}
	if !c.Verbose {
		t.Error("Verbose = false, want true")
	}
}

// TestApplyFile_AbsentKeysKeepDefaults is the precedence rule at the file
// level: a partial config file must not blank out everything it omits.
func TestApplyFile_AbsentKeysKeepDefaults(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, FileName, "build = \"make\"\n")

	c := Default()
	if err := c.ApplyFile(path); err != nil {
		t.Fatalf("ApplyFile: %v", err)
	}

	if c.Build != "make" {
		t.Errorf("Build = %q, want %q", c.Build, "make")
	}
	if c.Root != "." {
		t.Errorf("Root = %q, want the default %q", c.Root, ".")
	}
	if c.Debounce != DefaultDebounce {
		t.Errorf("Debounce = %v, want the default %v", c.Debounce, DefaultDebounce)
	}
	if c.KillDelay != DefaultKillDelay {
		t.Errorf("KillDelay = %v, want the default %v", c.KillDelay, DefaultKillDelay)
	}
}

// TestApplyFile_ExplicitFalseIsHonoured checks the pointer-field design: an
// explicit `verbose = false` is indistinguishable from absent unless absence
// is tracked separately.
func TestApplyFile_ExplicitFalseIsHonoured(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, FileName, "verbose = false\n")

	c := Default()
	c.Verbose = true // pretend something set it earlier
	if err := c.ApplyFile(path); err != nil {
		t.Fatalf("ApplyFile: %v", err)
	}
	if c.Verbose {
		t.Error("an explicit `verbose = false` should turn verbose off")
	}
}

// TestApplyFile_UnknownKeyIsRejected: a typo in a config file should be loud,
// not silently ignored.
func TestApplyFile_UnknownKeyIsRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, FileName, "buidl = \"make\"\n")

	c := Default()
	err := c.ApplyFile(path)
	if err == nil {
		t.Fatal("expected an error for an unknown config key")
	}
	if !strings.Contains(err.Error(), "buidl") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

func TestApplyFile_InvalidDuration(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"debounce", "debounce = \"soon\"\n", "debounce"},
		{"kill_delay", "kill_delay = \"ages\"\n", "kill_delay"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, t.TempDir(), FileName, tc.body)
			c := Default()
			err := c.ApplyFile(path)
			if err == nil {
				t.Fatal("expected an error for an unparseable duration")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should mention %q, got: %v", tc.want, err)
			}
		})
	}
}

func TestApplyFile_MalformedTOML(t *testing.T) {
	path := writeFile(t, t.TempDir(), FileName, "build = \n")
	c := Default()
	if err := c.ApplyFile(path); err == nil {
		t.Error("expected an error for malformed TOML")
	}
}

func TestApplyFile_MissingFile(t *testing.T) {
	c := Default()
	if err := c.ApplyFile(filepath.Join(t.TempDir(), "nope.toml")); err == nil {
		t.Error("expected an error for a missing config file")
	}
}

// --- FindFile ---------------------------------------------------------------

func TestFindFile(t *testing.T) {
	dir := t.TempDir()

	if _, ok := FindFile(dir); ok {
		t.Error("FindFile should report false when no config file exists")
	}

	writeFile(t, dir, FileName, "build = \"make\"\n")

	got, ok := FindFile(dir)
	if !ok {
		t.Fatal("FindFile should find the config file")
	}
	if got != filepath.Join(dir, FileName) {
		t.Errorf("FindFile = %q, want %q", got, filepath.Join(dir, FileName))
	}
}

func TestFindFile_IgnoresDirectoryWithConfigName(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, FileName), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, ok := FindFile(dir); ok {
		t.Error("a directory named like the config file must not be treated as one")
	}
}

// --- Normalize --------------------------------------------------------------

func TestNormalize_MakesRootAbsolute(t *testing.T) {
	c := Default()
	c.Root = "."
	if err := c.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if !filepath.IsAbs(c.Root) {
		t.Errorf("Root = %q, want an absolute path", c.Root)
	}
}

func TestNormalize_Extensions(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"go"}, ".go"},
		{[]string{".go"}, ".go"},
		{[]string{"GO"}, ".go"},
		{[]string{" .go ", "html"}, ".go,.html"},
		{[]string{"go", ".go", "GO"}, ".go"}, // duplicates collapse
		{[]string{"", ".", "  "}, ""},        // junk is dropped
	}

	for _, tc := range cases {
		c := Default()
		c.IncludeExt = tc.in
		if err := c.Normalize(); err != nil {
			t.Fatalf("Normalize(%v): %v", tc.in, err)
		}
		if got := strings.Join(c.IncludeExt, ","); got != tc.want {
			t.Errorf("Normalize(%v) IncludeExt = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalize_DirNames(t *testing.T) {
	c := Default()
	c.ExcludeDir = []string{" testdata ", "", "docs", "docs"}
	c.IncludeDir = []string{"build", " "}
	if err := c.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got := strings.Join(c.ExcludeDir, ","); got != "testdata,docs" {
		t.Errorf("ExcludeDir = %q, want %q", got, "testdata,docs")
	}
	if got := strings.Join(c.IncludeDir, ","); got != "build" {
		t.Errorf("IncludeDir = %q, want %q", got, "build")
	}
}

func TestNormalize_IsIdempotent(t *testing.T) {
	c := Default()
	c.Root = "."
	c.IncludeExt = []string{"go"}

	if err := c.Normalize(); err != nil {
		t.Fatalf("first Normalize: %v", err)
	}
	first := c
	if err := c.Normalize(); err != nil {
		t.Fatalf("second Normalize: %v", err)
	}

	if c.Root != first.Root {
		t.Errorf("Root changed on the second Normalize: %q -> %q", first.Root, c.Root)
	}
	if strings.Join(c.IncludeExt, ",") != ".go" {
		t.Errorf("IncludeExt = %v, want [.go]", c.IncludeExt)
	}
}

// --- Validate ---------------------------------------------------------------

func TestValidate_Accepts(t *testing.T) {
	c := Default()
	c.Root = t.TempDir()
	c.Build = "make"
	c.Exec = "./app"
	if err := c.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestValidate_Rejects(t *testing.T) {
	dir := t.TempDir()
	file := writeFile(t, dir, "notadir", "x")

	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"missing build", func(c *Config) { c.Build = "" }, "build command"},
		{"blank build", func(c *Config) { c.Build = "   " }, "build command"},
		{"missing exec", func(c *Config) { c.Exec = "" }, "exec command"},
		{"negative debounce", func(c *Config) { c.Debounce = -time.Second }, "debounce"},
		{"zero kill delay", func(c *Config) { c.KillDelay = 0 }, "kill-delay"},
		{"negative kill delay", func(c *Config) { c.KillDelay = -time.Second }, "kill-delay"},
		{"root does not exist", func(c *Config) { c.Root = filepath.Join(dir, "absent") }, "not readable"},
		{"root is a file", func(c *Config) { c.Root = file }, "not a directory"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			c.Root = dir
			c.Build = "make"
			c.Exec = "./app"
			tc.mutate(&c)

			err := c.Validate()
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should mention %q, got: %v", tc.want, err)
			}
		})
	}
}

// TestValidate_ZeroDebounceIsAllowed: zero means "rebuild immediately", which
// is a legitimate choice even if it is rarely what you want.
func TestValidate_ZeroDebounceIsAllowed(t *testing.T) {
	c := Default()
	c.Root = t.TempDir()
	c.Build = "make"
	c.Exec = "./app"
	c.Debounce = 0
	if err := c.Validate(); err != nil {
		t.Errorf("a zero debounce should be allowed, got: %v", err)
	}
}
