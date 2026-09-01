// Package lowerlogs persists structured lower-gateway logs outside SQLite.
// Files are bounded and rotated per source; SQLite only stores ingest cursors.
package lowerlogs

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	SourceID string
	Level    string
	PortID   string
	DeviceID string
	Query    string
	Limit    int
	Offset   int
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
	mu           sync.Mutex
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

	s.mu.Lock()
	defer s.mu.Unlock()
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
	file, err := os.OpenFile(current, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("open lower log file: %w", err)
	}
	if _, err = file.Write(encoded); err == nil {
		err = file.Sync()
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

func (s *Store) Query(filter Filter) (Page, error) {
	if strings.TrimSpace(filter.SourceID) == "" {
		return Page{}, errors.New("source_id is required")
	}
	if filter.Limit < 1 || filter.Limit > 1000 || filter.Offset < 0 {
		return Page{}, errors.New("invalid lower log pagination")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	files := s.filesNewestFirst(s.sourceDirectory(filter.SourceID))
	matched := make([]Event, 0, filter.Offset+filter.Limit)
	seen := make(map[string]struct{})
	skipped := 0
	for _, path := range files {
		events, invalid, err := readFile(path)
		if err != nil {
			return Page{}, err
		}
		skipped += invalid
		for index := len(events) - 1; index >= 0; index-- {
			event := events[index]
			if !matches(event, filter) {
				continue
			}
			key := event.SourceID + "\x00" + event.BootID + "\x00" + fmt.Sprint(event.Sequence)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			matched = append(matched, event)
		}
	}
	total := len(matched)
	start := min(filter.Offset, total)
	end := min(start+filter.Limit, total)
	items := append([]Event(nil), matched[start:end]...)
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

func readFile(path string) ([]Event, int, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("open lower log archive: %w", err)
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64<<10)
	result := make([]Event, 0)
	invalid := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if len(line) > maxJSONLineBytes {
				invalid++
			} else {
				var event Event
				if json.Unmarshal(line, &event) != nil {
					invalid++
				} else {
					result = append(result, event)
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, invalid, fmt.Errorf("read lower log archive: %w", readErr)
		}
	}
	return result, invalid, nil
}

func matches(event Event, filter Filter) bool {
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

// Stable ordering helper for tests and diagnostic tools that aggregate pages.
func SortChronological(events []Event) {
	sort.SliceStable(events, func(left, right int) bool {
		if events[left].Timestamp.Equal(events[right].Timestamp) {
			return events[left].Sequence < events[right].Sequence
		}
		return events[left].Timestamp.Before(events[right].Timestamp)
	})
}
