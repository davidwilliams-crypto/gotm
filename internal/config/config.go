// Package config holds user-tunable settings, persisted as JSON under
// %APPDATA%\gotm\config.json. The Settings tab edits it; it is also fine to
// edit by hand while gotm is closed.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"golang.org/x/sys/windows"
	"gotm/internal/safepath"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// Alert raises a tray notification when Metric stays above Above percent for
// ForSeconds. Metric is one of: cpu, mem, disk, gpu.
type Alert struct {
	Metric     string  `json:"metric"`
	Above      float64 `json:"above"`
	ForSeconds int     `json:"forSeconds"`
	Enabled    bool    `json:"enabled"`
}

// Config is the full settings document.
type Config struct {
	RefreshMs      int      `json:"refreshMs"`
	HistorySeconds int      `json:"historySeconds"`
	Columns        []string `json:"columns"`
	SortKey        string   `json:"sortKey"`
	SortDesc       bool     `json:"sortDesc"`
	GroupByName    bool     `json:"groupByName"`
	TreeView       bool     `json:"treeView"`
	HideSystemIdle bool     `json:"hideSystemIdle"`
	Alerts         []Alert  `json:"alerts"`
	RecorderDisk   bool     `json:"recorderDisk"`
	RyzenSDK       bool     `json:"ryzenSdk,omitempty"` // obsolete: the switch now lives in HKLM\SOFTWARE\gotm (admin only)
	WindowW        int      `json:"windowW"`
	WindowH        int      `json:"windowH"`
	LastPage       string   `json:"lastPage,omitempty"` // reopened at the next launch
}

// Default returns the settings used when no file exists yet.
func Default() Config {
	return Config{
		RefreshMs:      1000,
		HistorySeconds: 60,
		Columns:        []string{"name", "pid", "cpu", "mem", "io", "gpu", "threads", "user"},
		SortKey:        "cpu",
		SortDesc:       true,
		HideSystemIdle: true,
		Alerts: []Alert{
			{Metric: "cpu", Above: 90, ForSeconds: 15, Enabled: false},
			{Metric: "mem", Above: 90, ForSeconds: 30, Enabled: false},
		},
		WindowW: 1400,
		WindowH: 800,
	}
}

// Store is a concurrency-safe handle to the settings file.
type Store struct {
	mu  sync.RWMutex
	dir string
	cfg Config
}

// Dir returns the per-user config directory (created on demand). The roaming
// AppData folder comes from the shell, not the APPDATA variable, which any
// program can change for the user.
func Dir() string {
	base, err := windows.KnownFolderPath(windows.FOLDERID_RoamingAppData, 0)
	if err != nil || base == "" {
		base, _ = os.UserConfigDir()
	}
	d := filepath.Join(base, "gotm")
	if !safepath.Redirected(d) {
		_ = os.MkdirAll(d, 0o700)
	}
	return d
}

// ErrRedirected is returned instead of saving through a junction or link.
var ErrRedirected = errors.New("settings were not saved: gotm is running as administrator and the settings folder is a link to somewhere else")

// Open loads the user's settings, falling back to defaults for anything missing.
func Open() *Store { return OpenAt(Dir()) }

// OpenAt loads settings from dir (tests pass a temporary folder).
func OpenAt(dir string) *Store {
	s := &Store{dir: dir, cfg: Default()}
	if b, err := os.ReadFile(filepath.Join(s.dir, "config.json")); err == nil {
		// Notepad and Windows PowerShell write a UTF-8 BOM; encoding/json rejects it.
		b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
		loaded := Default()
		if json.Unmarshal(b, &loaded) == nil {
			s.cfg = loaded
		}
	}
	s.cfg = sanitize(s.cfg)
	return s
}

// Get returns a copy of the current settings.
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clone(s.cfg)
}

func clone(c Config) Config {
	c.Columns = slices.Clone(c.Columns)
	c.Alerts = slices.Clone(c.Alerts)
	return c
}

// Set validates, stores and persists new settings.
func (s *Store) Set(c Config) (Config, error) {
	c = sanitize(c)
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return c, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !safepath.WritableFile(filepath.Join(s.dir, "config.json")) {
		return clone(s.cfg), ErrRedirected
	}
	// Serialize writes and replace the file only after a complete, flushed write.
	// A failed save must leave both the disk and the running settings intact.
	f, err := os.CreateTemp(s.dir, "config-*.tmp")
	if err != nil {
		return clone(s.cfg), err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(s.dir, "config.json"))
	}
	if err != nil {
		return clone(s.cfg), err
	}
	s.cfg = c
	return clone(c), nil
}

// File returns the path of a file inside the config directory.
func (s *Store) File(name string) string { return filepath.Join(s.dir, name) }

func sanitize(c Config) Config {
	c = clone(c)
	d := Default()
	if c.RefreshMs < 250 || c.RefreshMs > 10000 {
		c.RefreshMs = d.RefreshMs
	}
	if c.HistorySeconds < 30 || c.HistorySeconds > 3600 {
		c.HistorySeconds = d.HistorySeconds
	}
	validColumns := map[string]bool{}
	for _, k := range []string{"name", "pid", "ppid", "cpu", "mem", "private", "io", "gpu", "threads", "handles", "prio", "user", "session", "start", "path"} {
		validColumns[k] = true
	}
	var cols []string
	seen := map[string]bool{"name": true}
	for _, k := range c.Columns {
		if validColumns[k] && !seen[k] {
			cols = append(cols, k)
			seen[k] = true
		}
	}
	if len(cols) == 0 && !containsName(c.Columns) {
		c.Columns = d.Columns
	} else {
		c.Columns = append([]string{"name"}, cols...)
	}
	if !validColumns[c.SortKey] {
		c.SortKey = d.SortKey
	}
	if c.Alerts == nil {
		c.Alerts = d.Alerts
	}
	alerts := make([]Alert, 0, len(c.Alerts))
	for _, al := range c.Alerts {
		if al.Metric != "cpu" && al.Metric != "mem" && al.Metric != "disk" && al.Metric != "gpu" {
			continue
		}
		if math.IsNaN(al.Above) || math.IsInf(al.Above, 0) {
			al.Above = 90
		}
		al.Above = math.Max(1, math.Min(100, al.Above))
		al.ForSeconds = max(1, min(3600, al.ForSeconds))
		alerts = append(alerts, al)
	}
	c.Alerts = alerts
	if c.WindowW < 640 || c.WindowW > 8000 {
		c.WindowW = d.WindowW
	}
	if c.WindowH < 420 || c.WindowH > 6000 {
		c.WindowH = d.WindowH
	}
	if c.TreeView && c.GroupByName {
		c.GroupByName = false // alternative layouts; tree wins
	}
	return c
}

func containsName(cols []string) bool {
	for _, k := range cols {
		if k == "name" {
			return true
		}
	}
	return false
}
