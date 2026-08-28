package logging

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRotatingWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manager.log")
	w, err := newRotatingWriter(path, 8, 2)
	if err != nil {
		t.Fatalf("newRotatingWriter() error = %v", err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("12345678")); err != nil {
		t.Fatalf("first Write() error = %v", err)
	}
	if _, err := w.Write([]byte("next")); err != nil {
		t.Fatalf("second Write() error = %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
}
