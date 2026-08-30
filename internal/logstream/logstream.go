// Package logstream tags the output of child processes so you can tell whose
// lines you are reading.
//
// Without it, your server's logs, the compiler's errors, and hotreload's own
// messages all land on the same stream with nothing to distinguish them, and
// working out which lines came from your application means reading carefully at
// exactly the moment you least want to.
//
// A Sink owns one underlying stream and hands out prefixing Writers that share
// its lock, so two tagged streams cannot interleave halfway through a line.
// Writers are line-buffered: child processes write in arbitrary chunks, and a
// prefix must only ever appear at the start of a line.
package logstream

import (
	"bytes"
	"io"
	"os"
	"strings"
	"sync"
)

// ANSI styling. Dim is used rather than a colour so the prefix stays visually
// subordinate to the output it is labelling.
const (
	ansiDim   = "\x1b[2m"
	ansiReset = "\x1b[0m"
)

// labelWidth is the width of the widest tag we emit, "[build]", so that
// differently-named streams stay aligned in the terminal.
const labelWidth = 7

// maxLineBuffer caps how much a Writer will hold while waiting for a newline.
// A process that emits a megabyte without one should still have its output
// shown rather than silently accumulated.
const maxLineBuffer = 64 << 10

// Sink serialises writes to a single underlying stream.
type Sink struct {
	mu  sync.Mutex
	out io.Writer
}

// NewSink returns a Sink writing to out.
func NewSink(out io.Writer) *Sink {
	return &Sink{out: out}
}

// Writer returns a line-prefixing writer for this sink. An empty prefix passes
// output through unchanged apart from the shared locking.
func (s *Sink) Writer(prefix string) *Writer {
	return &Writer{sink: s, prefix: prefix}
}

// Writer prefixes each complete line it receives before forwarding it.
// It is safe for concurrent use.
type Writer struct {
	sink   *Sink
	prefix string

	// buf holds an incomplete trailing line between calls. Guarded by sink.mu.
	buf []byte
}

// Write implements io.Writer. It always reports the full length as written:
// a child process should not fail because its log destination misbehaved.
func (w *Writer) Write(p []byte) (int, error) {
	w.sink.mu.Lock()
	defer w.sink.mu.Unlock()

	w.buf = append(w.buf, p...)

	var firstErr error
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := w.buf[:i+1]
		w.buf = w.buf[i+1:]
		if err := w.emitLocked(line); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// Nothing resembling a line has arrived in a long time; show it anyway.
	if len(w.buf) >= maxLineBuffer {
		if err := w.emitLocked(w.buf); err != nil && firstErr == nil {
			firstErr = err
		}
		w.buf = w.buf[:0]
	}

	return len(p), firstErr
}

// Flush writes any buffered partial line. Call it once a process has exited,
// since output without a trailing newline would otherwise be lost.
func (w *Writer) Flush() error {
	w.sink.mu.Lock()
	defer w.sink.mu.Unlock()

	if len(w.buf) == 0 {
		return nil
	}
	// Give the orphaned fragment a newline of its own so the next line does not
	// start mid-way across the terminal.
	line := append(append([]byte(nil), w.buf...), '\n')
	w.buf = w.buf[:0]
	return w.emitLocked(line)
}

// emitLocked writes one line, prefix included. Callers must hold sink.mu.
func (w *Writer) emitLocked(line []byte) error {
	if w.prefix == "" {
		_, err := w.sink.out.Write(line)
		return err
	}
	out := make([]byte, 0, len(w.prefix)+len(line))
	out = append(out, w.prefix...)
	out = append(out, line...)
	_, err := w.sink.out.Write(out)
	return err
}

// Prefix builds a padded tag such as "[build] ". When colour is true the tag is
// dimmed and reset, so the labelled output keeps whatever styling it chose for
// itself.
func Prefix(label string, colour bool) string {
	tag := "[" + label + "]"
	if len(tag) < labelWidth {
		tag += strings.Repeat(" ", labelWidth-len(tag))
	}
	tag += " "

	if !colour {
		return tag
	}
	return ansiDim + tag + ansiReset
}

// SupportsColour reports whether ANSI styling should be written to f.
//
// It respects the NO_COLOR convention and TERM=dumb, requires f to be a
// character device rather than a pipe or file, and on Windows enables virtual
// terminal processing. That last part matters: without it a Windows console
// prints escape sequences literally, so the prefixes would arrive as visible
// garbage.
//
// It has a side effect on Windows (setting the console mode) and is intended to
// be called once during start-up.
func SupportsColour(f *os.File) bool {
	if f == nil {
		return false
	}
	// https://no-color.org: any non-empty value disables colour.
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if term := os.Getenv("TERM"); term == "dumb" {
		return false
	}

	info, err := f.Stat()
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		// Redirected to a file or pipe; escape codes would just be noise.
		return false
	}

	return enableVirtualTerminal(f)
}
