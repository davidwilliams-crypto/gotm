//go:build windows

package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type blockedRecordingFile struct {
	bytes.Buffer
	entered chan struct{}
	release chan struct{}
}

func (f *blockedRecordingFile) Write(b []byte) (int, error) {
	select {
	case f.entered <- struct{}{}:
	default:
	}
	<-f.release
	return f.Buffer.Write(b)
}
func (f *blockedRecordingFile) Close() error { return nil }

func TestRecorderCaptureAndToggleDoNotWaitForWrite(t *testing.T) {
	r := NewRecorder(time.Hour, time.Second, t.TempDir())
	f := &blockedRecordingFile{entered: make(chan struct{}, 1), release: make(chan struct{})}
	base := time.Now().UnixMilli()
	r.file, r.fileDay = f, time.UnixMilli(base).Format("2006-01-02")
	r.SetPersist(true)
	r.SetEnabled(true)
	added := make(chan struct{})
	go func() { r.Add(snap(base, 10)); close(added) }()
	<-f.entered
	toggled := make(chan struct{})
	go func() { r.SetPersist(false); close(toggled) }()
	status := make(chan struct{})
	go func() { r.DiskErr(); r.Samples(0, 0); close(status) }()
	for name, done := range map[string]<-chan struct{}{"capture": added, "toggle": toggled, "status": status} {
		select {
		case <-done:
		case <-time.After(150 * time.Millisecond):
			t.Errorf("%s blocked on recording disk write", name)
		}
	}
	if r.Len() != 1 {
		t.Error("sample missing from memory")
	}
	close(f.release)
	<-added
	<-toggled
	closeRecorder(t, r)
}

func flushRecorder(t *testing.T, r *Recorder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}

func closeRecorder(t *testing.T, r *Recorder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Error(err)
	}
}

func TestRecorderBlockedBacklogBoundedAndDrainedInOrder(t *testing.T) {
	r := NewRecorder(time.Hour, time.Second, t.TempDir())
	f := &blockedRecordingFile{entered: make(chan struct{}, 1), release: make(chan struct{})}
	base := time.Now().Truncate(24 * time.Hour).Add(time.Hour).UnixMilli()
	r.file, r.fileDay = f, time.UnixMilli(base).Format("2006-01-02")
	r.SetPersist(true)
	r.SetEnabled(true)
	r.Add(snap(base, 0))
	<-f.entered
	for i := 1; i <= 300; i++ {
		r.Add(snap(base+int64(i)*1000, float64(i)))
	}
	if r.Len() != 301 {
		t.Fatalf("memory recording lost samples: %d", r.Len())
	}
	r.mu.RLock()
	pending, capacity := r.diskCount, len(r.diskQueue)
	r.mu.RUnlock()
	if pending != 256 || capacity != 256 {
		t.Fatalf("unbounded backlog: %d/%d", pending, capacity)
	}
	if err := r.DiskErr(); err == nil || !strings.Contains(err.Error(), "44 samples") {
		t.Fatalf("disk gap not reported: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := r.Flush(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked flush: %v", err)
	}
	r.SetPersist(false)
	r.SetPersist(true) // pending writes remain ordered across quick toggles
	close(f.release)
	flushRecorder(t, r)
	r.Add(snap(base+301000, 301))
	closeRecorder(t, r)
	lines := bytes.Split(bytes.TrimSpace(f.Bytes()), []byte{'\n'})
	if len(lines) != 258 {
		t.Fatalf("saved %d accepted samples, want 258", len(lines))
	}
	for i, line := range lines {
		var s RecSample
		if err := json.Unmarshal(line, &s); err != nil {
			t.Fatal(err)
		}
		want := i
		if i == 257 {
			want = 301
		}
		if s.CPU != float64(want) {
			t.Fatalf("sample %d out of order: %v", i, s.CPU)
		}
	}
	r.SetPersist(true)
	r.Add(snap(base+302000, 302))
	if r.Persist() || r.Len() != 302 {
		t.Fatal("closed recorder accepted new work")
	}
}

func TestRecorderCloseDeadlineDoesNotAbandonWorkerOwnership(t *testing.T) {
	r := NewRecorder(time.Hour, time.Second, t.TempDir())
	f := &blockedRecordingFile{entered: make(chan struct{}, 1), release: make(chan struct{})}
	base := time.Now().UnixMilli()
	r.file, r.fileDay = f, time.UnixMilli(base).Format("2006-01-02")
	r.SetPersist(true)
	r.SetEnabled(true)
	r.Add(snap(base, 10))
	<-f.entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := r.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked close: %v", err)
	}
	close(f.release)
	closeRecorder(t, r)
	if f.Len() == 0 {
		t.Fatal("accepted sample was not drained after disk recovered")
	}
}
