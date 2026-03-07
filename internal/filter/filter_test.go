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
		got := shouldIgnoreDir(tc.path)
		if got != tc.ignore {
			t.Errorf("shouldIgnoreDir(%q) = %v, want %v", tc.path, got, tc.ignore)
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
		got := shouldIgnoreFile(tc.path)
		if got != tc.ignore {
			t.Errorf("shouldIgnoreFile(%q) = %v, want %v", tc.path, got, tc.ignore)
		}
	}
}

// TestExportedFunctions verifies the exported wrappers delegate correctly.
func TestExportedFunctions(t *testing.T) {
	// ShouldIgnoreDir wraps shouldIgnoreDir
	if !ShouldIgnoreDir(".git") {
		t.Error("ShouldIgnoreDir(.git) should return true")
	}
	if ShouldIgnoreDir("src") {
		t.Error("ShouldIgnoreDir(src) should return false")
	}
	// ShouldIgnoreFile wraps shouldIgnoreFile
	if !ShouldIgnoreFile("main.go~") {
		t.Error("ShouldIgnoreFile(main.go~) should return true")
	}
	if ShouldIgnoreFile("main.go") {
		t.Error("ShouldIgnoreFile(main.go) should return false")
	}
}

// TestBinDirNotWatched is an important regression test: if someone does
//   hotreload --root . --build "go build -o ./bin/server ."
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
		if !shouldIgnoreFile(p) {
			t.Errorf("expected %q to be ignored (build artefact)", p)
		}
	}
}
