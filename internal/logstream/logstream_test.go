package logstream

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestWriter_PrefixesCompleteLines(t *testing.T) {
	var buf bytes.Buffer
	w := NewSink(&buf).Writer("[app] ")

	if _, err := w.Write([]byte("first\nsecond\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	want := "[app] first\n[app] second\n"
	if got := buf.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestWriter_BuffersPartialLines is the reason this exists. A child process
// writes in whatever chunks it likes, so a prefix must only appear at the start
// of a line, never in the middle of one that arrived in pieces.
func TestWriter_BuffersPartialLines(t *testing.T) {
	var buf bytes.Buffer
	w := NewSink(&buf).Writer("[app] ")

	for _, chunk := range []string{"hel", "lo wor", "ld"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write(%q): %v", chunk, err)
		}
		if buf.Len() != 0 {
			t.Fatalf("nothing should be emitted before a newline, got %q", buf.String())
		}
	}

	if _, err := w.Write([]byte("\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, want := buf.String(), "[app] hello world\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestWriter_FlushEmitsTrailingFragment covers output with no trailing newline,
// which is common when a process dies mid-write.
func TestWriter_FlushEmitsTrailingFragment(t *testing.T) {
	var buf bytes.Buffer
	w := NewSink(&buf).Writer("[app] ")

	if _, err := w.Write([]byte("panic: nil pointer")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected nothing before Flush, got %q", buf.String())
	}

	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got, want := buf.String(), "[app] panic: nil pointer\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// Flushing again must not repeat it.
	buf.Reset()
	if err := w.Flush(); err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("second Flush emitted %q, want nothing", buf.String())
	}
}

func TestWriter_EmptyPrefixPassesThrough(t *testing.T) {
	var buf bytes.Buffer
	w := NewSink(&buf).Writer("")

	if _, err := w.Write([]byte("plain\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, want := buf.String(), "plain\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWriter_PreservesBlankLines(t *testing.T) {
	var buf bytes.Buffer
	w := NewSink(&buf).Writer("[b] ")

	if _, err := w.Write([]byte("a\n\nb\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, want := buf.String(), "[b] a\n[b] \n[b] b\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestWriter_ReportsFullLength matters because os/exec copies output in a loop
// and a short write would be treated as an error against the child.
func TestWriter_ReportsFullLength(t *testing.T) {
	w := NewSink(&bytes.Buffer{}).Writer("[app] ")

	payload := []byte("some output\nand more")
	n, err := w.Write(payload)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(payload) {
		t.Errorf("Write returned %d, want %d", n, len(payload))
	}
}

// TestWriter_HugeLineIsNotHeldForever guards the buffer cap: a process emitting
// megabytes without a newline should still have its output shown.
func TestWriter_HugeLineIsNotHeldForever(t *testing.T) {
	var buf bytes.Buffer
	w := NewSink(&buf).Writer("[app] ")

	if _, err := w.Write(bytes.Repeat([]byte("x"), maxLineBuffer+10)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if buf.Len() == 0 {
		t.Error("a line exceeding the buffer cap should be emitted rather than accumulated")
	}
}

// TestSink_SerialisesWriters is the point of sharing a sink: two labelled
// streams must not interleave halfway through a line.
func TestSink_SerialisesWriters(t *testing.T) {
	var buf bytes.Buffer
	sink := NewSink(&buf)
	app := sink.Writer("[app] ")
	build := sink.Writer("[build] ")

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = app.Write([]byte("app line\n"))
		}()
		go func() {
			defer wg.Done()
			_, _ = build.Write([]byte("build line\n"))
		}()
	}
	wg.Wait()

	for _, line := range strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n") {
		if line != "[app] app line" && line != "[build] build line" {
			t.Fatalf("interleaved output: %q", line)
		}
	}
}

// --- Prefix -----------------------------------------------------------------

func TestPrefix_PadsToAConsistentWidth(t *testing.T) {
	build := Prefix("build", false)
	app := Prefix("app", false)

	if len(build) != len(app) {
		t.Errorf("prefixes should align: %q (%d) vs %q (%d)", build, len(build), app, len(app))
	}
	if !strings.HasPrefix(build, "[build]") {
		t.Errorf("Prefix(build) = %q, want it to start with [build]", build)
	}
	if !strings.HasPrefix(app, "[app]") {
		t.Errorf("Prefix(app) = %q, want it to start with [app]", app)
	}
}

func TestPrefix_ColourWrapsAndResets(t *testing.T) {
	plain := Prefix("app", false)
	if strings.Contains(plain, "\x1b") {
		t.Errorf("Prefix with colour off should contain no escape codes, got %q", plain)
	}

	coloured := Prefix("app", true)
	if !strings.HasPrefix(coloured, ansiDim) {
		t.Errorf("coloured prefix should start dimmed, got %q", coloured)
	}
	// The reset must come before the content, so labelled output keeps whatever
	// styling it chose for itself.
	if !strings.HasSuffix(coloured, ansiReset) {
		t.Errorf("coloured prefix should reset before the output, got %q", coloured)
	}
}

// --- SupportsColour ---------------------------------------------------------

func TestSupportsColour_NilFile(t *testing.T) {
	if SupportsColour(nil) {
		t.Error("SupportsColour(nil) should be false")
	}
}

func TestSupportsColour_RespectsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	// A regular file is not a terminal either, but NO_COLOR must win regardless.
	f, err := createTempFile(t)
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	if SupportsColour(f) {
		t.Error("NO_COLOR should disable colour")
	}
}

func TestSupportsColour_RejectsNonTerminal(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")

	f, err := createTempFile(t)
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	if SupportsColour(f) {
		t.Error("a regular file is not a terminal; colour should be disabled")
	}
}

func TestSupportsColour_RespectsDumbTerm(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")

	f, err := createTempFile(t)
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	if SupportsColour(f) {
		t.Error("TERM=dumb should disable colour")
	}
}
