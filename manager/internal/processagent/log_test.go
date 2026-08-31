package processagent

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"rcd_manager/internal/domain"
)

func logFixture(t *testing.T) *rotatingLog {
	t.Helper()
	log, err := openLog(t.TempDir(), "lower.log")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log
}

func writeLog(t *testing.T, log *rotatingLog, data []byte) {
	t.Helper()
	if n, err := log.Write(data); err != nil || n != len(data) {
		t.Fatalf("log write n=%d want=%d err=%v", n, len(data), err)
	}
}

func TestRotatingLogAppendsExistingOutputAndTails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lower.log"), []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	log, err := openLog(dir, "lower.log")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	writeLog(t, log, []byte("new\n"))
	for _, test := range []struct {
		limit     int
		text      string
		truncated bool
	}{{64, "old\nnew\n", false}, {8, "old\nnew\n", false}, {4, "new\n", true}} {
		got, err := log.tail("lower-a", test.limit)
		if err != nil || got.InstanceID != "lower-a" || got.Text != test.text || got.Truncated != test.truncated || got.SizeBytes != 8 {
			t.Fatalf("tail(%d)=%#v err=%v", test.limit, got, err)
		}
	}
}

func TestRotatingLogSingleWriteCrossesMultipleBoundedRotations(t *testing.T) {
	log := logFixture(t)
	data := make([]byte, 3*processLogLimit+7)
	for index := range data {
		data[index] = byte('a' + index/processLogLimit)
	}
	writeLog(t, log, data)
	current, err := os.ReadFile(log.path)
	if err != nil || string(current) != "ddddddd" {
		t.Fatalf("current output=%q err=%v", current, err)
	}
	previous, err := os.ReadFile(log.path + ".1")
	if err != nil || len(previous) != processLogLimit || bytes.Count(previous, []byte{'c'}) != processLogLimit {
		t.Fatalf("previous rotation size=%d err=%v", len(previous), err)
	}
	entries, err := os.ReadDir(filepath.Dir(log.path))
	if err != nil || len(entries) != 2 {
		t.Fatalf("must retain exactly current+one previous output: entries=%v err=%v", entries, err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || info.Size() > processLogLimit {
			t.Fatalf("unbounded retained log %s: info=%v err=%v", entry.Name(), info, err)
		}
	}
}

func TestRotatingLogTailUTF8BoundariesAndInvalidOutput(t *testing.T) {
	for _, test := range []struct {
		name      string
		data      []byte
		limit     int
		want      string
		truncated bool
	}{
		{"complete multibyte", []byte("甲乙"), 16, "甲乙", false},
		{"exact multibyte boundary", []byte("甲乙"), 3, "乙", true},
		{"tail begins inside a rune", []byte("prefix甲乙丙"), 5, "丙", true},
		{"tail contains no complete rune", []byte("甲乙"), 2, "", true},
		{"invalid bytes replaced", []byte{0xff, 'a', 0xc0, 0xaf, 'b'}, 16, "�a�b", false},
		{"incomplete final rune replaced", []byte{'a', 0xe4, 0xb8}, 16, "a�", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			log := logFixture(t)
			writeLog(t, log, test.data)
			got, err := log.tail("site-a", test.limit)
			if err != nil || got.InstanceID != "site-a" || got.Text != test.want || got.SizeBytes != int64(len(test.data)) || got.Truncated != test.truncated {
				t.Fatalf("tail=%#v want=%q truncated=%v err=%v", got, test.want, test.truncated, err)
			}
			if !utf8.ValidString(got.Text) || len(got.Text) > test.limit {
				t.Fatalf("tail is invalid UTF8 or exceeds byte budget: len=%d limit=%d", len(got.Text), test.limit)
			}
		})
	}
}

func TestRotatingLogTailBoundsUTF8ReplacementExpansion(t *testing.T) {
	log := logFixture(t)
	const suffix = "\nEND"
	// Each invalid byte followed by ASCII expands from two to four UTF-8
	// bytes. Even when the raw file fits the request, the returned JSON text
	// must remain bounded and keep the newest (tail) output.
	data := append(bytes.Repeat([]byte{0xff, 'a'}, (domain.MaxProcessLogBytes-len(suffix))/2), suffix...)
	writeLog(t, log, data)
	got, err := log.tail("lower-a", domain.MaxProcessLogBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(got.Text) || len(got.Text) > domain.MaxProcessLogBytes || !got.Truncated || !strings.HasSuffix(got.Text, suffix) || got.SizeBytes != int64(len(data)) {
		t.Fatalf("expanded tail must remain bounded, valid, and newest-first: len=%d limit=%d truncated=%v hasSuffix=%v size=%d", len(got.Text), domain.MaxProcessLogBytes, got.Truncated, strings.HasSuffix(got.Text, suffix), got.SizeBytes)
	}
}

func TestRotatingLogRotationFailureCanRecoverWithoutLosingCurrentOutput(t *testing.T) {
	log := logFixture(t)
	writeLog(t, log, bytes.Repeat([]byte{'a'}, processLogLimit))
	// A directory at the fixed archive name deterministically prevents a
	// rename, even if the test runs as root (chmod would not do so).
	if err := os.Mkdir(log.path+".1", 0o750); err != nil {
		t.Fatal(err)
	}
	if n, err := log.Write([]byte("next")); err == nil || n != 0 {
		t.Fatalf("blocked rotation n=%d err=%v", n, err)
	}
	current, err := os.ReadFile(log.path)
	if err != nil || len(current) != processLogLimit || bytes.Count(current, []byte{'a'}) != processLogLimit {
		t.Fatalf("failed rotation lost the current output: size=%d err=%v", len(current), err)
	}
	if err := os.Remove(log.path + ".1"); err != nil {
		t.Fatal(err)
	}
	writeLog(t, log, []byte("next"))
	previous, err := os.ReadFile(log.path + ".1")
	if err != nil || !bytes.Equal(previous, current) {
		t.Fatalf("recovered rotation lost existing output: size=%d err=%v", len(previous), err)
	}
	got, err := log.tail("lower-a", 16)
	if err != nil || got.Text != "next" {
		t.Fatalf("writer stayed poisoned after transient failure: %#v err=%v", got, err)
	}
}

func TestRotatingLogClosedAtBoundaryCannotReopenOrRotate(t *testing.T) {
	log := logFixture(t)
	writeLog(t, log, bytes.Repeat([]byte{'a'}, processLogLimit))
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := log.Write([]byte("after-close")); n != 0 || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed writer reopened at its rotation boundary: n=%d err=%v", n, err)
	}
	entries, err := os.ReadDir(filepath.Dir(log.path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("post-close write changed log files: entries=%v err=%v", entries, err)
	}
	data, err := os.ReadFile(log.path)
	if err != nil || len(data) != processLogLimit || bytes.Count(data, []byte{'a'}) != processLogLimit {
		t.Fatalf("post-close write altered completed output: size=%d err=%v", len(data), err)
	}
}

func TestRotatingLogConcurrentWritesTailsAndClose(t *testing.T) {
	log := logFixture(t)
	const workers = 4
	var writers, firstWrites sync.WaitGroup
	writers.Add(workers)
	firstWrites.Add(workers)
	errorsSeen := make(chan error, workers+1)
	for worker := range workers {
		go func() {
			defer writers.Done()
			for index := range 200 {
				line := []byte(fmt.Sprintf("worker=%d item=%03d\n", worker, index))
				n, err := log.Write(line)
				if index == 0 {
					firstWrites.Done()
				}
				if errors.Is(err, os.ErrClosed) && n == 0 {
					return
				}
				if err != nil || n != len(line) {
					errorsSeen <- fmt.Errorf("worker=%d write=%d n=%d err=%v", worker, index, n, err)
					return
				}
			}
		}()
	}
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for range 100 {
			got, err := log.tail("lower-a", 128)
			if err != nil || !utf8.ValidString(got.Text) || len(got.Text) > 128 {
				errorsSeen <- fmt.Errorf("concurrent tail len=%d err=%v", len(got.Text), err)
				return
			}
		}
	}()
	firstWrites.Wait()
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	writers.Wait()
	<-readerDone
	close(errorsSeen)
	for err := range errorsSeen {
		t.Error(err)
	}
	data, err := os.ReadFile(log.path)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var worker, index int
		if _, err := fmt.Sscanf(line, "worker=%d item=%d", &worker, &index); err != nil || worker < 0 || worker >= workers || index < 0 || index >= 200 || seen[line] || line != fmt.Sprintf("worker=%d item=%03d", worker, index) {
			t.Fatalf("log write was interleaved, duplicated, or incomplete: %q", line)
		}
		seen[line] = true
	}
	if len(seen) < workers {
		t.Fatalf("completed writes disappeared: %d", len(seen))
	}
	if n, err := log.Write([]byte("after-close")); n != 0 || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("post-close write n=%d err=%v", n, err)
	}
}

func TestRotatingLogOpenRejectsUnavailableDirectory(t *testing.T) {
	if log, err := openLog(filepath.Join(t.TempDir(), "missing"), "lower.log"); err == nil {
		_ = log.Close()
		t.Fatal("log open unexpectedly created an unconfigured directory")
	}
}
