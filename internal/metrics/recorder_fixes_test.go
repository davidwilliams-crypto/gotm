//go:build windows

package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCsvCellNeutralizesFormulaPrefixes(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""}, {"plain", "plain"},
		{"=1+1", "'=1+1"}, {"+SUM(A1)", "'+SUM(A1)"}, {"-x", "'-x"}, {"@cmd", "'@cmd"},
	} {
		if got := csvCell(c.in); got != c.want {
			t.Errorf("csvCell(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExportCSVNeutralizesFormulaNames(t *testing.T) {
	r := NewRecorder(time.Hour, time.Second, "")
	r.SetEnabled(true)
	base := time.Now().Add(-5 * time.Second).UnixMilli()
	r.Add(snap(base, 20, Proc{PID: 7, Name: "=HYPERLINK(\"http://x\")-t.exe", CPU: 99}))
	r.SetEnabled(false)
	out := filepath.Join(t.TempDir(), "rec.csv")
	if err := r.ExportCSV(out, 0, 0); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `'=HYPERLINK`) {
		t.Fatalf("a formula-looking process name reached the export unneutralized:\n%s", b)
	}
}

func TestRecorderReportsAndRecoversFromDiskWriteFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rec")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	r := NewRecorder(time.Hour, time.Second, dir)
	t.Cleanup(func() { r.SetPersist(false) })
	r.SetPersist(true)
	r.SetEnabled(true)
	base := time.Now().Add(-5 * time.Second).UnixMilli()
	// A directory where the day file would open makes every open fail.
	day := time.UnixMilli(base).Format("2006-01-02")
	if err := os.Mkdir(filepath.Join(dir, day+".jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	r.Add(snap(base, 10))
	if err := r.DiskErr(); err == nil {
		t.Fatal("a failing disk write was not reported")
	}
	// The next sample retries the open, and a good write clears the error.
	if err := os.Remove(filepath.Join(dir, day+".jsonl")); err != nil {
		t.Fatal(err)
	}
	r.Add(snap(base+2000, 11))
	if err := r.DiskErr(); err != nil {
		t.Fatalf("write error did not clear after a successful write: %v", err)
	}
	if r.Len() != 2 {
		t.Fatalf("samples held: %d, want 2", r.Len())
	}
}

func TestRecorderLoadDiskSurvivesOversizedLine(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-4 * time.Second).UnixMilli()
	r := NewRecorder(time.Hour, time.Second, dir)
	t.Cleanup(func() { r.SetPersist(false) })
	r.SetPersist(true)
	r.SetEnabled(true)
	r.Add(snap(base, 10))
	r.Add(snap(base+2000, 20))
	r.SetEnabled(false)
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("recording files: %v, %v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	// a >1 MiB junk line, then one more good sample after it
	huge := strings.Repeat("x", 1<<21)
	extra, _ := json.Marshal(RecSample{T: base + 4000, CPU: 30})
	blob := append(append(append([]byte{}, data...), huge...), '\n')
	blob = append(blob, extra...)
	blob = append(blob, '\n')
	if err := os.WriteFile(files[0], blob, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded := NewRecorder(time.Hour, time.Second, dir)
	if n := loaded.LoadDisk(time.Hour); n != 3 {
		t.Fatalf("loaded %d samples, want 3: the samples after an oversized line were dropped", n)
	}
}
