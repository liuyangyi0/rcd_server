// Package lowerlogs persists structured lower-gateway logs outside SQLite.
// Files are bounded and rotated per source; SQLite only stores ingest cursors.
package lowerlogs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxFileBytes = int64(8 << 20)
	defaultKeepFiles    = 8
	maxJSONLineBytes    = 64 << 10
)

type Event struct {
	SourceID   string         `json:"source_id"`
	BootID     string         `json:"boot_id"`
	Sequence   uint64         `json:"sequence"`
	Timestamp  time.Time      `json:"timestamp"`
	ReceivedAt time.Time      `json:"received_at"`
	Level      string         `json:"level"`
	Component  string         `json:"component"`
	PortID     string         `json:"port_id,omitempty"`
	DeviceID   string         `json:"device_id,omitempty"`
	Message    string         `json:"message"`
	Fields     map[string]any `json:"fields,omitempty"`
}

type Filter struct {
	CommandAttempts string
	SourceID        string
	Level           string
	PortID          string
	DeviceID        string
	Query           string
	From            time.Time
	To              time.Time
	Limit           int
	Offset          int
}

type Page struct {
	Items   []Event
	Total   int
	Skipped int
}

type Store struct {
	root         string
	maxFileBytes int64
	keepFiles    int
	locks        sync.Map // source ID -> *sync.Mutex; never held while scanning
}

func (s *Store) sourceLock(sourceID string) *sync.Mutex {
	value, _ := s.locks.LoadOrStore(sourceID, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func Open(root string) (*Store, error) {
	return openWithOptions(root, defaultMaxFileBytes, defaultKeepFiles)
}

func openWithOptions(root string, maxFileBytes int64, keepFiles int) (*Store, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("lower log directory is required")
	}
	if maxFileBytes < 1024 || keepFiles < 1 {
		return nil, errors.New("invalid lower log rotation options")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("create lower log directory: %w", err)
	}
	return &Store{root: root, maxFileBytes: maxFileBytes, keepFiles: keepFiles}, nil
}

func (s *Store) Append(events []Event) error {
	if len(events) == 0 {
		return nil
	}
	sourceID := events[0].SourceID
	var encoded []byte
	for _, event := range events {
		if event.SourceID != sourceID {
			return errors.New("one lower log append must contain exactly one source")
		}
		line, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("encode lower log event: %w", err)
		}
		if len(line) > maxJSONLineBytes {
			return errors.New("encoded lower log event exceeds 64 KiB")
		}
		encoded = append(encoded, line...)
		encoded = append(encoded, '\n')
	}

	mu := s.sourceLock(sourceID)
	mu.Lock()
	defer mu.Unlock()
	directory := s.sourceDirectory(sourceID)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("create source lower log directory: %w", err)
	}
	current := filepath.Join(directory, "current.jsonl")
	if info, err := os.Stat(current); err == nil && info.Size()+int64(len(encoded)) > s.maxFileBytes {
		if err := s.rotate(directory); err != nil {
			return err
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat lower log file: %w", err)
	}
	file, err := os.OpenFile(current, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o640)
	if err != nil {
		return fmt.Errorf("open lower log file: %w", err)
	}
	originalSize, err := repairIncompleteTail(file)
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("repair lower log append boundary: %w", err)
	}
	if _, err = file.Write(encoded); err == nil {
		err = file.Sync()
	}
	if err != nil {
		// The batch was not acknowledged. Restore the previous complete-line
		// boundary so retransmission cannot glue its first event to a half line.
		rollbackErr := file.Truncate(originalSize)
		if rollbackErr == nil {
			rollbackErr = file.Sync()
		}
		err = errors.Join(err, rollbackErr)
	}
	closeErr := file.Close()
	if err != nil {
		return fmt.Errorf("append lower log events: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close lower log file: %w", closeErr)
	}
	return nil
}

func repairIncompleteTail(file *os.File) (int64, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	size := info.Size()
	if size == 0 {
		return 0, nil
	}
	last := []byte{0}
	if _, err := file.ReadAt(last, size-1); err != nil {
		return 0, err
	}
	if last[0] == '\n' {
		return size, nil
	}
	buf := make([]byte, 64<<10)
	boundary := int64(0)
	for end := size; end > 0; {
		start := max(int64(0), end-int64(len(buf)))
		n, err := file.ReadAt(buf[:end-start], start)
		if err != nil {
			return 0, err
		}
		if at := bytes.LastIndexByte(buf[:n], '\n'); at >= 0 {
			boundary = start + int64(at) + 1
			break
		}
		end = start
	}
	if err := file.Truncate(boundary); err != nil {
		return 0, err
	}
	if err := file.Sync(); err != nil {
		return 0, err
	}
	return boundary, nil
}

func (s *Store) Query(filter Filter) (Page, error) {
	return s.QueryContext(context.Background(), filter)
}

func (s *Store) QueryContext(parent context.Context, filter Filter) (Page, error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	if strings.TrimSpace(filter.SourceID) == "" {
		return Page{}, errors.New("source_id is required")
	}
	if filter.Limit < 1 || filter.Limit > 1000 || filter.Offset < 0 {
		return Page{}, errors.New("invalid lower log pagination")
	}
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	files, err := s.snapshotContext(ctx, filter.SourceID)
	if err != nil {
		return Page{}, err
	}
	defer func() {
		for _, file := range files {
			_ = file.file.Close()
		}
	}()
	items := make([]Event, 0, filter.Limit)
	seen := make(map[string]struct{})
	skipped := 0
	total := 0
	for _, file := range files {
		err := scanReverse(ctx, file, func(line []byte, oversized bool) {
			var event Event
			if oversized || json.Unmarshal(line, &event) != nil {
				skipped++
				return
			}
			if !matches(event, filter) {
				return
			}
			key := event.SourceID + "\x00" + event.BootID + "\x00" + fmt.Sprint(event.Sequence)
			if _, exists := seen[key]; exists {
				return
			}
			seen[key] = struct{}{}
			if total >= filter.Offset && len(items) < filter.Limit {
				items = append(items, event)
			}
			total++
		})
		if err != nil {
			return Page{}, err
		}
	}
	return Page{Items: items, Total: total, Skipped: skipped}, nil
}

func (s *Store) sourceDirectory(sourceID string) string {
	hash := sha256.Sum256([]byte(sourceID))
	prefix := make([]rune, 0, 24)
	for _, character := range sourceID {
		if len(prefix) >= 24 {
			break
		}
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' {
			prefix = append(prefix, character)
		} else {
			prefix = append(prefix, '_')
		}
	}
	name := string(prefix)
	if name == "" {
		name = "source"
	}
	return filepath.Join(s.root, name+"-"+hex.EncodeToString(hash[:6]))
}

func (s *Store) rotate(directory string) error {
	oldest := filepath.Join(directory, fmt.Sprintf("archive-%d.jsonl", s.keepFiles))
	if err := os.Remove(oldest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove expired lower log archive: %w", err)
	}
	for index := s.keepFiles - 1; index >= 1; index-- {
		from := filepath.Join(directory, fmt.Sprintf("archive-%d.jsonl", index))
		to := filepath.Join(directory, fmt.Sprintf("archive-%d.jsonl", index+1))
		if err := os.Rename(from, to); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rotate lower log archive: %w", err)
		}
	}
	current := filepath.Join(directory, "current.jsonl")
	if err := os.Rename(current, filepath.Join(directory, "archive-1.jsonl")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rotate current lower log: %w", err)
	}
	return nil
}

func (s *Store) filesNewestFirst(directory string) []string {
	files := make([]string, 0, s.keepFiles+1)
	current := filepath.Join(directory, "current.jsonl")
	if _, err := os.Stat(current); err == nil {
		files = append(files, current)
	}
	for index := 1; index <= s.keepFiles; index++ {
		path := filepath.Join(directory, fmt.Sprintf("archive-%d.jsonl", index))
		if _, err := os.Stat(path); err == nil {
			files = append(files, path)
		}
	}
	return files
}

func matches(event Event, filter Filter) bool {
	if filter.CommandAttempts != "" && !matchesCommandAttempts(event, filter.CommandAttempts) {
		return false
	}
	if !filter.From.IsZero() && event.Timestamp.Before(filter.From) {
		return false
	}
	if !filter.To.IsZero() && event.Timestamp.After(filter.To) {
		return false
	}
	if filter.Level != "" && !strings.EqualFold(event.Level, filter.Level) {
		return false
	}
	if filter.PortID != "" && event.PortID != filter.PortID {
		return false
	}
	if filter.DeviceID != "" && event.DeviceID != filter.DeviceID {
		return false
	}
	if query := strings.ToLower(strings.TrimSpace(filter.Query)); query != "" {
		haystack := strings.ToLower(strings.Join([]string{event.Message, event.Component, event.PortID, event.DeviceID}, " "))
		if !strings.Contains(haystack, query) {
			return false
		}
	}
	return true
}

// Filter before counting/paging. Do not infer attempts by matching payloads or
// timestamps: two managers or Sessions may issue the same bytes independently.
func matchesCommandAttempts(event Event, filter string) bool {
	if event.Component != "rcd_server::command_audit" {
		return false
	}
	var value struct {
		SchemaVersion int     `json:"schema_version"`
		SentCount     *uint32 `json:"sent_count"`
		AttemptCount  uint32  `json:"attempt_count"`
		MaxAttempts   uint32  `json:"max_attempts"`
		Result        string  `json:"result"`
	}
	if json.Unmarshal([]byte(event.Message), &value) != nil || value.SchemaVersion != 1 || value.SentCount == nil ||
		*value.SentCount > value.AttemptCount || value.AttemptCount > value.MaxAttempts {
		return false
	}
	switch value.Result {
	case "received", "failed", "expired", "interrupted":
	default:
		return false
	}
	switch filter {
	case "all":
		return true
	case "retried":
		return *value.SentCount > 1
	case "three":
		return *value.SentCount >= 3
	default:
		return false
	}
}

// Stable ordering helper for tests and diagnostic tools that aggregate pages.
func SortChronological(events []Event) {
	sort.SliceStable(events, func(left, right int) bool {
		if events[left].Timestamp.Equal(events[right].Timestamp) {
			return events[left].Sequence < events[right].Sequence
		}
		return events[left].Timestamp.Before(events[right].Timestamp)
	})
}
