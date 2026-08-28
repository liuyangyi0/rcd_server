package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

const (
	defaultMaxBytes = int64(10 * 1024 * 1024)
	defaultBackups  = 5
)

func Setup(dir string, development bool) (*slog.Logger, io.Closer, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, nil, fmt.Errorf("create log directory: %w", err)
	}
	writer, err := newRotatingWriter(filepath.Join(dir, "rcd-manager.log"), defaultMaxBytes, defaultBackups)
	if err != nil {
		return nil, nil, err
	}
	level := slog.LevelInfo
	if development {
		level = slog.LevelDebug
	}
	handler := slog.NewJSONHandler(io.MultiWriter(os.Stderr, writer), &slog.HandlerOptions{Level: level})
	return slog.New(handler), writer, nil
}

type rotatingWriter struct {
	mu         sync.Mutex
	path       string
	maxBytes   int64
	maxBackups int
	file       *os.File
	size       int64
}

func newRotatingWriter(path string, maxBytes int64, maxBackups int) (*rotatingWriter, error) {
	w := &rotatingWriter{path: path, maxBytes: maxBytes, maxBackups: maxBackups}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) Write(value []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.size > 0 && w.size+int64(len(value)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(value)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *rotatingWriter) open() error {
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return fmt.Errorf("stat log file: %w", err)
	}
	w.file = file
	w.size = info.Size()
	return nil
}

func (w *rotatingWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("close log before rotation: %w", err)
	}
	w.file = nil
	if w.maxBackups > 0 {
		_ = os.Remove(fmt.Sprintf("%s.%d", w.path, w.maxBackups))
		for index := w.maxBackups - 1; index >= 1; index-- {
			oldPath := fmt.Sprintf("%s.%d", w.path, index)
			newPath := fmt.Sprintf("%s.%d", w.path, index+1)
			if _, err := os.Stat(oldPath); err == nil {
				if err := os.Rename(oldPath, newPath); err != nil {
					return fmt.Errorf("rotate log backup: %w", err)
				}
			}
		}
		if _, err := os.Stat(w.path); err == nil {
			if err := os.Rename(w.path, w.path+".1"); err != nil {
				return fmt.Errorf("rotate active log: %w", err)
			}
		}
	}
	return w.open()
}
