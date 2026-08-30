package filter

import "testing"

func TestShouldIgnoreDir(t *testing.T) {
	cases := []struct {
		path   string
		ignore bool
	}{
		// Version control
		{".git", true},
		{".git/objects", true},
		{"src/.git", true},
		{".svn", true},
		{".hg", true},
		// JS ecosystems
		{"node_modules", true},
		{"node_modules/react", true},
		// Go / build output
		{"vendor", true},
		{"bin", true},
		{"bin/server", true},
		{"dist", true},
		{"build", true},
		// Any hidden directory
		{".hidden", true},
		{".idea", true},
		{".vscode", true},
		// Should NOT be ignored
		{"src/main", false},
		{"internal/controller", false},
		{"testserver", false},
		{"cmd/hotreload", false},
		{"bindings", false}, // prefix "bin" but full name is different
	}

	for _, tc := range cases {
		got := ShouldIgnoreDir(tc.path)
		if got != tc.ignore {
			t.Errorf("ShouldIgnoreDir(%q) = %v, want %v", tc.path, got, tc.ignore)
		}
	}
}

func TestShouldIgnoreFile(t *testing.T) {
	cases := []struct {
		path   string
		ignore bool
	}{
		// Vim swap files
		{"main.go.swp", true},
		{"main.go.swx", true},
		// Backup / temp files
		{"main.go~", true},
		{"main.go.bak", true},
		{"main.go.tmp", true},
		// Emacs temp files
		{".#main.go", true},
		{"#main.go#", true},
		// Hidden files
		{".hidden_file", true},
		// Files inside ignored directories
		{".git/COMMIT_EDITMSG", true},
		{"node_modules/foo/index.js", true},
		{"bin/server", true},
		{"vendor/github.com/pkg/main.go", true},
		// Compiled artefacts
		{"main.o", true},
		{"libfoo.a", true},
		{"myprogram.test", true}, // go test -c output
		{"libfoo.so", true},
		// Should NOT be ignored
		{"main.go", false},
		{"go.mod", false},
		{"go.sum", false},
		{"internal/controller/controller.go", false},
		{"config.yaml", false},
		{"README.md", false},
	}

	for _, tc := range cases {
		got := ShouldIgnoreFile(tc.path)
		if got != tc.ignore {
			t.Errorf("ShouldIgnoreFile(%q) = %v, want %v", tc.path, got, tc.ignore)
		}
	}
}

// TestPackageFunctionsUseDefaults verifies the package-level helpers delegate
// to a Filter built with no overrides.
func TestPackageFunctionsUseDefaults(t *testing.T) {
	if !ShouldIgnoreDir(".git") {
		t.Error("ShouldIgnoreDir(.git) should return true")
	}
	if ShouldIgnoreDir("src") {
		t.Error("ShouldIgnoreDir(src) should return false")
	}
	if !ShouldIgnoreFile("main.go~") {
		t.Error("ShouldIgnoreFile(main.go~) should return true")
	}
	if ShouldIgnoreFile("main.go") {
		t.Error("ShouldIgnoreFile(main.go) should return false")
	}
}

// TestBinDirNotWatched is an important regression test: if someone does
//
//	hotreload --root . --build "go build -o ./bin/server ."
//
// the binary being written to ./bin/server must NOT trigger a rebuild loop.
func TestBinDirNotWatched(t *testing.T) {
	paths := []string{
		"bin/server",
		"bin/myapp",
		"bin/server.test",
		"dist/bundle.js",
		"build/output",
	}
	for _, p := range paths {
		if !ShouldIgnoreFile(p) {
			t.Errorf("expected %q to be ignored (build artefact)", p)
		}
	}
}

// --- IncludeExt -------------------------------------------------------------

func TestIncludeExt_RestrictsToListedExtensions(t *testing.T) {
	f := New(Options{IncludeExt: []string{".go", ".html"}})

	allowed := []string{"main.go", "internal/x/handler.go", "views/index.html"}
	for _, p := range allowed {
		if f.ShouldIgnoreFile(p) {
			t.Errorf("expected %q to be allowed by include-ext", p)
		}
	}

	// The whole point: docs and configs no longer trigger a compile.
	ignored := []string{"README.md", "config.yaml", "go.mod", "Makefile", "data.json"}
	for _, p := range ignored {
		if !f.ShouldIgnoreFile(p) {
			t.Errorf("expected %q to be ignored when include-ext is set", p)
		}
	}
}

func TestIncludeExt_EmptyAllowsEverything(t *testing.T) {
	f := New(Options{})
	for _, p := range []string{"README.md", "main.go", "config.yaml", "Makefile"} {
		if f.ShouldIgnoreFile(p) {
			t.Errorf("expected %q to be allowed when include-ext is empty", p)
		}
	}
}

// TestIncludeExt_StillHonoursArtefactRules makes sure the allow-list does not
// become a way for swap files and hidden files to sneak through.
func TestIncludeExt_StillHonoursArtefactRules(t *testing.T) {
	f := New(Options{IncludeExt: []string{".go"}})
	ignored := []string{".main.go.swp", ".#main.go", "main.go~", "bin/thing.go", ".hidden/main.go"}
	for _, p := range ignored {
		if !f.ShouldIgnoreFile(p) {
			t.Errorf("expected %q to stay ignored even though .go is included", p)
		}
	}
}

// TestIncludeExt_OverridesArtefactExtension covers the deliberate escape
// hatch: asking for .so files explicitly wins over the artefact ignore list.
func TestIncludeExt_OverridesArtefactExtension(t *testing.T) {
	f := New(Options{IncludeExt: []string{".so"}})
	if f.ShouldIgnoreFile("plugin.so") {
		t.Error("explicitly including .so should override the artefact ignore list")
	}
}

func TestIncludeExt_IsCaseInsensitiveOnTheFileSide(t *testing.T) {
	f := New(Options{IncludeExt: []string{".go"}})
	if f.ShouldIgnoreFile("MAIN.GO") {
		t.Error("extension matching should be case-insensitive")
	}
}

// --- ExcludeDir -------------------------------------------------------------

func TestExcludeDir_AddsToIgnoreSet(t *testing.T) {
	f := New(Options{ExcludeDir: []string{"testdata", "docs"}})

	for _, p := range []string{"testdata", "internal/testdata", "docs/guide"} {
		if !f.ShouldIgnoreDir(p) {
			t.Errorf("expected dir %q to be ignored via exclude-dir", p)
		}
	}
	if !f.ShouldIgnoreFile("internal/testdata/fixture.go") {
		t.Error("files inside an excluded dir should be ignored")
	}
	// Unrelated directories are untouched.
	if f.ShouldIgnoreDir("internal/controller") {
		t.Error("exclude-dir should not affect unrelated directories")
	}
}

// --- IncludeDir -------------------------------------------------------------

// TestIncludeDir_UnIgnoresBuiltInDir is the escape hatch for projects whose
// sources genuinely live in a directory hotreload ignores by default.
func TestIncludeDir_UnIgnoresBuiltInDir(t *testing.T) {
	f := New(Options{IncludeDir: []string{"build"}})

	if f.ShouldIgnoreDir("build") {
		t.Error("include-dir should un-ignore the built-in 'build' entry")
	}
	if f.ShouldIgnoreFile("build/main.go") {
		t.Error("files under an included dir should not be ignored")
	}
	// Sibling defaults stay ignored.
	if !f.ShouldIgnoreDir("dist") {
		t.Error("including 'build' must not un-ignore 'dist'")
	}
}

// TestIncludeDir_UnIgnoresDotDir covers the all-dot-directories rule, which
// is otherwise impossible to opt out of.
func TestIncludeDir_UnIgnoresDotDir(t *testing.T) {
	f := New(Options{IncludeDir: []string{".config"}})

	if f.ShouldIgnoreDir(".config") {
		t.Error("include-dir should override the dot-directory rule")
	}
	if f.ShouldIgnoreDir("src/.config/nested") {
		t.Error("include-dir should apply to any position in the path")
	}
	// Other dot-directories are still ignored.
	if !f.ShouldIgnoreDir(".git") {
		t.Error("including .config must not un-ignore .git")
	}
}

// TestIncludeDir_DoesNotDefeatFileRules confirms a dotfile inside an included
// directory is still a dotfile.
func TestIncludeDir_DoesNotDefeatFileRules(t *testing.T) {
	f := New(Options{IncludeDir: []string{".config"}})
	if !f.ShouldIgnoreFile(".config/.hidden") {
		t.Error("hidden files stay ignored even inside an included directory")
	}
	if f.ShouldIgnoreFile(".config/settings.go") {
		t.Error("a normal file inside an included directory should be allowed")
	}
}

func TestIncludeDir_BeatsExcludeDir(t *testing.T) {
	// Contradictory input: include wins, and that is documented behaviour.
	f := New(Options{ExcludeDir: []string{"build"}, IncludeDir: []string{"build"}})
	if f.ShouldIgnoreDir("build") {
		t.Error("include-dir should take precedence over exclude-dir")
	}
}

// --- misc -------------------------------------------------------------------

func TestIncludeExts_Reporting(t *testing.T) {
	if got := Default().IncludeExts(); got != nil {
		t.Errorf("Default().IncludeExts() = %v, want nil", got)
	}
	f := New(Options{IncludeExt: []string{".go", ".tmpl"}})
	if got := f.IncludeExts(); len(got) != 2 {
		t.Errorf("IncludeExts() = %v, want 2 entries", got)
	}
}

func TestShouldIgnoreDir_ParentTraversalIsNotIgnored(t *testing.T) {
	// ".." must not be treated as a hidden directory.
	if ShouldIgnoreDir("../sibling") {
		t.Error(`".." should not be treated as a hidden directory`)
	}
}
