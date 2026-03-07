package filter

import "testing"

func TestShouldIgnoreDir(t *testing.T) {
	cases := []struct {
		path   string
		ignore bool
	}{

		{".git", true},
		{".git/objects", true},
		{"src/.git", true},
		{".svn", true},
		{".hg", true},

		{"node_modules", true},
		{"node_modules/react", true},

		{"vendor", true},
		{"bin", true},
		{"bin/server", true},
		{"dist", true},
		{"build", true},

		{".hidden", true},
		{".idea", true},
		{".vscode", true},

		{"src/main", false},
		{"internal/controller", false},
		{"testserver", false},
		{"cmd/hotreload", false},
		{"bindings", false},
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

		{"main.go.swp", true},
		{"main.go.swx", true},

		{"main.go~", true},
		{"main.go.bak", true},
		{"main.go.tmp", true},

		{".#main.go", true},
		{"#main.go#", true},

		{".hidden_file", true},

		{".git/COMMIT_EDITMSG", true},
		{"node_modules/foo/index.js", true},
		{"bin/server", true},
		{"vendor/github.com/pkg/main.go", true},

		{"main.o", true},
		{"libfoo.a", true},
		{"myprogram.test", true},
		{"libfoo.so", true},

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

func TestExportedFunctions(t *testing.T) {

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
