package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSanitizeRepairsBadValues(t *testing.T) {
	c := sanitize(Config{RefreshMs: 5, HistorySeconds: 99999, WindowW: 3, TreeView: true, GroupByName: true})
	d := Default()
	if c.RefreshMs != d.RefreshMs || c.HistorySeconds != d.HistorySeconds || c.WindowW != d.WindowW {
		t.Fatalf("not repaired: %+v", c)
	}
	if len(c.Columns) == 0 || c.SortKey == "" {
		t.Fatalf("columns/sort not defaulted: %+v", c)
	}
	if c.TreeView && c.GroupByName {
		t.Fatal("tree and group must be mutually exclusive")
	}
}

func TestFailedSaveKeepsLastSettings(t *testing.T) {
	dir := t.TempDir() // never the real profile
	s := OpenAt(dir)
	before := s.Get()
	// A directory at the destination makes replacing config.json fail.
	if err := os.Mkdir(s.File("config.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	c := s.Get()
	c.RefreshMs = 2000
	if _, err := s.Set(c); err == nil {
		t.Fatal("save unexpectedly succeeded")
	}
	if got := s.Get(); got.RefreshMs != before.RefreshMs {
		t.Fatalf("failed save changed running settings: %d", got.RefreshMs)
	}
}

func TestStoreOwnsSavedSlices(t *testing.T) {
	dir := t.TempDir() // never the real profile
	s := OpenAt(dir)
	c := s.Get()
	saved, err := s.Set(c)
	if err != nil {
		t.Fatal(err)
	}
	c.Columns[0] = "broken"
	c.Alerts[0].Enabled = true
	saved.Columns[1] = "broken"
	saved.Alerts[1].Enabled = true
	got := s.Get()
	if got.Columns[0] != "name" || got.Columns[1] != "pid" || got.Alerts[0].Enabled || got.Alerts[1].Enabled {
		t.Fatalf("caller mutated stored settings: %+v", got)
	}
}

func TestExplicitEmptyAlertsStayEmpty(t *testing.T) {
	c := Default()
	c.Alerts = []Alert{}
	if got := sanitize(c); got.Alerts == nil || len(got.Alerts) != 0 {
		t.Fatalf("explicit empty alerts replaced: %+v", got.Alerts)
	}
}

func TestMalformedConfigDoesNotApplyPartialValues(t *testing.T) {
	dir := t.TempDir() // never the real profile
	s := OpenAt(dir)
	if err := os.WriteFile(s.File("config.json"), []byte(`{"refreshMs":2000,"treeView":"wrong type"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := OpenAt(dir).Get(); got.RefreshMs != 1000 || got.TreeView {
		t.Fatalf("partially applied malformed settings: %+v", got)
	}
}

func TestSanitizeUnknownColumnsSortAndAlerts(t *testing.T) {
	c := Default()
	c.Columns = []string{"pid", "pid", "unknown"}
	c.SortKey = "unknown"
	c.Alerts = []Alert{{Metric: "cpu", Above: 150, ForSeconds: -1}, {Metric: "unknown", Above: 90, ForSeconds: 10}}
	got := sanitize(c)
	if len(got.Columns) != 2 || got.Columns[0] != "name" || got.Columns[1] != "pid" || got.SortKey != "cpu" {
		t.Fatalf("bad process settings: %+v", got)
	}
	if len(got.Alerts) != 1 || got.Alerts[0].Above != 100 || got.Alerts[0].ForSeconds != 1 {
		t.Fatalf("bad alert settings: %+v", got.Alerts)
	}
}

func TestConcurrentSavesLeaveValidMatchingFile(t *testing.T) {
	dir := t.TempDir() // never the real profile
	s := OpenAt(dir)
	done := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func(i int) {
			c := s.Get()
			c.RefreshMs = 500 + i*250
			_, err := s.Set(c)
			done <- err
		}(i)
	}
	for i := 0; i < 20; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(s.File("config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var disk Config
	if err := json.Unmarshal(b, &disk); err != nil {
		t.Fatal(err)
	}
	if disk.RefreshMs != s.Get().RefreshMs {
		t.Fatalf("file=%d, memory=%d", disk.RefreshMs, s.Get().RefreshMs)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir() // never the real profile
	s := OpenAt(dir)
	c := s.Get()
	c.RefreshMs = 2000
	c.TreeView = true
	if _, err := s.Set(c); err != nil {
		t.Fatal(err)
	}
	if got := OpenAt(dir).Get(); got.RefreshMs != 2000 || !got.TreeView {
		t.Fatalf("not persisted: %+v", got)
	}
}

// Notepad and Windows PowerShell save JSON with a UTF-8 BOM; it must not make
// the whole file silently fall back to defaults.
func TestOpenAcceptsUTF8BOM(t *testing.T) {
	dir := t.TempDir() // never the real profile
	body := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"refreshMs": 2000, "treeView": true}`)...)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	got := OpenAt(dir).Get()
	if got.RefreshMs != 2000 || !got.TreeView {
		t.Fatalf("BOM file ignored: %+v", got)
	}
}

// Settings live in the shell's roaming AppData folder; the APPDATA variable,
// which any program can change for the user, must not move them.
func TestDirIgnoresAPPDATAVariable(t *testing.T) {
	t.Setenv("APPDATA", t.TempDir())
	if d := Dir(); filepath.Dir(d) == os.Getenv("APPDATA") {
		t.Fatalf("Dir() followed the APPDATA variable: %s", d)
	}
}
