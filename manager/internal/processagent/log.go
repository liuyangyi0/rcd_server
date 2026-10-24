package processagent

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"rcd_manager/internal/domain"
)

const processLogLimit = 5 << 20

// rotatingLog keeps only the current file and one previous file. Neither the
// HTTP API nor a command payload can select a filesystem path.
type rotatingLog struct {
	mu     sync.Mutex
	path   string
	file   *os.File
	size   int64
	closed bool
}

func openLog(dir, name string) (*rotatingLog, error) {
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &rotatingLog{path: path, file: f, size: info.Size()}, nil
}

func (l *rotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, os.ErrClosed
	}
	total := 0
	for len(p) > 0 {
		if l.size >= processLogLimit {
			// Linux permits renaming an open file. Keep the original descriptor
			// usable if rotation fails so a transient filesystem problem does
			// not permanently disable output from an otherwise live child.
			if err := os.Rename(l.path, l.path+".1"); err != nil {
				return total, err
			}
			f, err := os.OpenFile(l.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
			if err != nil {
				_ = os.Rename(l.path+".1", l.path)
				return total, err
			}
			_ = l.file.Close()
			l.file, l.size = f, 0
		}
		count := min(len(p), processLogLimit-int(l.size))
		n, err := l.file.Write(p[:count])
		total += n
		l.size += int64(n)
		p = p[n:]
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func (l *rotatingLog) tail(instance string, limit int) (domain.ProcessLog, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(l.path)
	if err != nil {
		return domain.ProcessLog{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return domain.ProcessLog{}, err
	}
	offset := max(int64(0), info.Size()-int64(limit))
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return domain.ProcessLog{}, err
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)))
	if err != nil {
		return domain.ProcessLog{}, err
	}
	// A byte-tail may begin halfway through UTF-8. Omit leading continuation
	// bytes; invalid child output is replaced instead of producing invalid JSON.
	for offset > 0 && len(data) > 0 && data[0]&0xc0 == 0x80 {
		data = data[1:]
	}
	text := strings.ToValidUTF8(string(data), "�")
	truncated := offset > 0
	if len(text) > limit {
		text = text[len(text)-limit:]
		for len(text) > 0 && !utf8.RuneStart(text[0]) {
			text = text[1:]
		}
		truncated = true
	}
	return domain.ProcessLog{InstanceID: instance, Text: text, Truncated: truncated, SizeBytes: info.Size()}, nil
}

func (l *rotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	return l.file.Close()
}
