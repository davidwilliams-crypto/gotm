//go:build windows

package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type closingRecordingFile struct {
	bytes.Buffer
	closing chan struct{}
	release chan struct{}
}

func (f *closingRecordingFile) Close() error { close(f.closing); <-f.release; return nil }

func TestRecorderReenableDuringBlockedClose(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(time.Hour, time.Second, dir)
	f := &closingRecordingFile{closing: make(chan struct{}), release: make(chan struct{})}
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local).UnixMilli()
	day := time.UnixMilli(base).Format("2006-01-02")
	r.file, r.fileDay = f, day
	r.SetEnabled(true)
	r.SetPersist(true)
	r.Add(snap(base, 10))
	flushRecorder(t, r)
	disabled := make(chan struct{})
	go func() { r.SetPersist(false); close(disabled) }()
	select {
	case <-disabled:
	case <-time.After(time.Second):
		close(f.release)
		t.Fatal("disable waited on close")
	}
	select {
	case <-f.closing:
	case <-time.After(time.Second):
		close(f.release)
		t.Fatal("worker did not close file")
	}
	resumed := make(chan struct{})
	go func() { r.SetPersist(true); r.Add(snap(base+1000, 20)); close(resumed) }()
	select {
	case <-resumed:
	case <-time.After(time.Second):
		close(f.release)
		t.Fatal("resume/capture waited on close")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if err := r.Flush(ctx); err == nil {
		cancel()
		close(f.release)
		t.Fatal("flush completed before queued sample was attempted")
	}
	cancel()
	close(f.release)
	closeRecorder(t, r)
	if r.Len() != 2 {
		t.Fatal("memory lost accepted sample")
	}
	data, err := os.ReadFile(filepath.Join(dir, day+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var got RecSample
	if err := json.Unmarshal(bytes.TrimSpace(data), &got); err != nil || got.CPU != 20 {
		t.Fatalf("queued sample lost during close/reopen: %s, %v", data, err)
	}
}

func TestRecorderOverflowSurvivesSuccessfulDrainUntilRestart(t *testing.T) {
	r := NewRecorder(time.Minute, time.Second, t.TempDir())
	f := &blockedRecordingFile{entered: make(chan struct{}, 1), release: make(chan struct{})}
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local).UnixMilli()
	r.file, r.fileDay = f, time.UnixMilli(base).Format("2006-01-02")
	r.SetEnabled(true)
	r.SetPersist(true)
	r.Add(snap(base, 0))
	select {
	case <-f.entered:
	case <-time.After(time.Second):
		close(f.release)
		t.Fatal("write not started")
	}
	for i := 1; i <= 61; i++ {
		r.Add(snap(base+int64(i)*1000, float64(i)))
	}
	close(f.release)
	flushRecorder(t, r)
	if err := r.DiskErr(); err == nil || !strings.Contains(err.Error(), "1 samples") {
		t.Fatalf("successful drain hid disk gap: %v", err)
	}
	r.SetPersist(true)
	if r.DiskErr() == nil {
		t.Fatal("redundant enable cleared overflow")
	}
	r.SetPersist(false)
	r.SetPersist(true)
	if err := r.DiskErr(); err != nil {
		t.Fatalf("explicit restart did not reset gap: %v", err)
	}
	closeRecorder(t, r)
}
