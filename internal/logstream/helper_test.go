package logstream

import (
	"os"
	"path/filepath"
	"testing"
)

// createTempFile returns an open regular file, which is deliberately not a
// character device, so the terminal detection can be exercised.
func createTempFile(t *testing.T) (*os.File, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.log")
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, nil
}
