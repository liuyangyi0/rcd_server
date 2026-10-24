package lowerlogs

import (
	"context"
	"io"
	"os"
	"time"
)

// Open descriptors pin the original segments across rename/unlink. The size
// boundary also prevents an append from entering a partially completed page.
type fileSnapshot struct {
	file *os.File
	size int64
}

func (s *Store) snapshot(source string) (result []fileSnapshot, err error) {
	return s.snapshotContext(context.Background(), source)
}

func (s *Store) snapshotContext(ctx context.Context, source string) (result []fileSnapshot, err error) {
	mu := s.sourceLock(source)
	for !mu.TryLock() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	defer mu.Unlock()
	defer func() {
		if err != nil {
			for _, f := range result {
				_ = f.file.Close()
			}
		}
	}()
	for _, path := range s.filesNewestFirst(s.sourceDirectory(source)) {
		var f *os.File
		f, err = openSnapshotFile(path)
		if err != nil {
			return
		}
		var info os.FileInfo
		info, err = f.Stat()
		if err != nil {
			_ = f.Close()
			return
		}
		result = append(result, fileSnapshot{f, info.Size()})
	}
	return
}

// Read backwards in fixed chunks, retaining at most one bounded JSON line.
// Oversized/corrupt lines count as invalid, without unbounded ReadBytes growth.
func scanReverse(ctx context.Context, f fileSnapshot, visit func([]byte, bool)) error {
	buf := make([]byte, 64<<10)
	line := make([]byte, 0, 1024)
	oversized, trailing := false, true
	emit := func() {
		for i, j := 0, len(line)-1; i < j; i, j = i+1, j-1 {
			line[i], line[j] = line[j], line[i]
		}
		visit(line, oversized)
		line = line[:0]
		oversized = false
	}
	for end := f.size; end > 0; {
		if err := ctx.Err(); err != nil {
			return err
		}
		start := max(int64(0), end-int64(len(buf)))
		n, err := f.file.ReadAt(buf[:end-start], start)
		if err != nil && err != io.EOF {
			return err
		}
		if int64(n) != end-start {
			return io.ErrUnexpectedEOF
		}
		for i := n - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				if trailing {
					trailing = false
					continue
				}
				emit()
			} else {
				trailing = false
				if len(line) < maxJSONLineBytes {
					line = append(line, buf[i])
				} else {
					oversized = true
				}
			}
		}
		end = start
	}
	if f.size > 0 {
		emit()
	}
	return ctx.Err()
}
