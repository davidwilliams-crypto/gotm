//go:build windows

package metrics

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"gotm/internal/safepath"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RecProc is one process in a recorded sample.
type RecProc struct {
	PID  uint32  `json:"pid"`
	Name string  `json:"name"`
	CPU  float64 `json:"cpu"`
	Mem  uint64  `json:"mem"`
}

// RecSample is one moment of the flight recording: the headline numbers plus
// the processes that were busiest, so a spike can be explained afterwards.
type RecSample struct {
	T       int64     `json:"t"`
	CPU     float64   `json:"cpu"`
	Mem     float64   `json:"mem"`
	GPU     float64   `json:"gpu"`
	DiskP   float64   `json:"diskP"`
	DiskBps float64   `json:"diskBps"`
	NetBps  float64   `json:"netBps"`
	Top     []RecProc `json:"top"`
}

// Mark is a user bookmark on the timeline.
type Mark struct {
	T     int64  `json:"t"`
	Label string `json:"label"`
}

// Recorder keeps a rolling window of RecSamples in memory and can optionally
// append them to disk. It is fed from the sampler, one Add per tick.
type Recorder struct {
	mu       sync.RWMutex
	ring     []RecSample
	capacity int
	every    time.Duration
	last     int64
	enabled  bool
	marks    []Mark

	persist bool
	dir     string
	file    *os.File
	fileDay string
}

// NewRecorder keeps `retention` of history, one sample per `every`.
func NewRecorder(retention, every time.Duration, dir string) *Recorder {
	if every < time.Second {
		every = time.Second
	}
	n := int(retention / every)
	if n < 60 {
		n = 60
	}
	return &Recorder{capacity: n, every: every, enabled: true, dir: dir}
}

func (r *Recorder) SetEnabled(v bool) { r.mu.Lock(); r.enabled = v; r.mu.Unlock() }
func (r *Recorder) Enabled() bool     { r.mu.RLock(); defer r.mu.RUnlock(); return r.enabled }

// SetPersist turns on appending samples to <dir>\yyyy-mm-dd.jsonl.
func (r *Recorder) SetPersist(v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.persist = v
	if !v && r.file != nil {
		_ = r.file.Close()
		r.file, r.fileDay = nil, ""
	}
}
func (r *Recorder) Persist() bool { r.mu.RLock(); defer r.mu.RUnlock(); return r.persist }

// Add records a snapshot if enabled and the interval has elapsed.
func (r *Recorder) Add(s *Snapshot) {
	if s == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.T < r.last {
		r.last = 0 // the clock stepped backwards: keep recording rather than pause until it catches up
	}
	if !r.enabled || s.T-r.last < r.every.Milliseconds() {
		return
	}
	r.last = s.T

	rs := RecSample{T: s.T, CPU: s.CPU, Mem: round1(s.Mem.Percent), GPU: s.GPU.Percent,
		DiskP: s.Disk.Percent, DiskBps: s.Disk.ReadBps + s.Disk.WriteBps, NetBps: s.Net.RxBps + s.Net.TxBps}
	rs.Top = topProcs(s.Procs)

	r.ring = append(r.ring, rs)
	if len(r.ring) > r.capacity {
		r.ring = append([]RecSample(nil), r.ring[len(r.ring)-r.capacity:]...)
	}
	if r.persist {
		r.appendDisk(rs)
	}
}

func topProcs(procs []Proc) []RecProc {
	idx := make([]int, 0, len(procs))
	for i := range procs {
		if procs[i].PID != 0 {
			idx = append(idx, i)
		}
	}
	pick := map[int]bool{}
	sort.Slice(idx, func(a, b int) bool { return procs[idx[a]].CPU > procs[idx[b]].CPU })
	for i := 0; i < len(idx) && i < 10; i++ {
		pick[idx[i]] = true
	}
	sort.Slice(idx, func(a, b int) bool { return procs[idx[a]].Mem > procs[idx[b]].Mem })
	for i := 0; i < len(idx) && i < 3; i++ {
		pick[idx[i]] = true
	}
	out := make([]RecProc, 0, len(pick))
	for i := range pick {
		p := procs[i]
		out = append(out, RecProc{PID: p.PID, Name: p.Name, CPU: p.CPU, Mem: p.Mem})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].CPU > out[b].CPU })
	return out
}

func (r *Recorder) appendDisk(rs RecSample) {
	if r.dir == "" {
		return
	}
	day := time.UnixMilli(rs.T).Format("2006-01-02")
	if r.file == nil || r.fileDay != day {
		if r.file != nil {
			_ = r.file.Close()
		}
		path := filepath.Join(r.dir, day+".jsonl")
		if !safepath.WritableFile(path) {
			r.file = nil
			return // elevated, and the folder or file is a link: do not write through it
		}
		_ = os.MkdirAll(r.dir, 0o700)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			r.file = nil
			return
		}
		r.file, r.fileDay = f, day
	}
	if b, err := json.Marshal(rs); err == nil {
		_, _ = r.file.Write(append(b, '\n'))
	}
}

// LoadDisk reloads samples from the last `window` of on-disk files.
func (r *Recorder) LoadDisk(window time.Duration) int {
	if r.dir == "" {
		return 0
	}
	files, _ := filepath.Glob(filepath.Join(r.dir, "*.jsonl"))
	sort.Strings(files)
	now := time.Now().UnixMilli()
	cut := now - window.Milliseconds()
	// Files are named by day (2006-01-02.jsonl); skip days before the window
	// instead of parsing tens of megabytes to keep three hours.
	firstDay := time.UnixMilli(cut).Add(-24 * time.Hour).Format("2006-01-02")
	kept := files[:0]
	for _, fn := range files {
		if day := strings.TrimSuffix(filepath.Base(fn), ".jsonl"); len(day) != 10 || day >= firstDay {
			kept = append(kept, fn)
		}
	}
	files = kept
	var loaded []RecSample
	for _, fn := range files {
		f, err := os.Open(fn)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var rs RecSample
			if json.Unmarshal(sc.Bytes(), &rs) == nil && rs.T >= cut && rs.T <= now {
				loaded = append(loaded, rs)
			}
		}
		f.Close()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// A replay may overlap the in-memory recording, and a file may have an
	// out-of-order tail after a clock adjustment. Range lookup requires ordered,
	// unique timestamps. Prefer an existing live sample when timestamps overlap.
	byTime := make(map[int64]RecSample, len(loaded)+len(r.ring))
	for _, s := range loaded {
		byTime[s.T] = s
	}
	for _, s := range r.ring {
		byTime[s.T] = s
	}
	r.ring = make([]RecSample, 0, len(byTime))
	for _, s := range byTime {
		r.ring = append(r.ring, s)
	}
	sort.Slice(r.ring, func(i, j int) bool { return r.ring[i].T < r.ring[j].T })
	if len(r.ring) > r.capacity {
		r.ring = r.ring[len(r.ring)-r.capacity:]
	}
	if len(r.ring) > 0 {
		r.last = r.ring[len(r.ring)-1].T
	}
	return len(loaded)
}

// PruneDisk deletes recording files older than maxAge.
func (r *Recorder) PruneDisk(maxAge time.Duration) {
	if safepath.Redirected(r.dir) {
		return // never delete through a link while elevated
	}
	files, _ := filepath.Glob(filepath.Join(r.dir, "*.jsonl"))
	for _, fn := range files {
		if st, err := os.Stat(fn); err == nil && time.Since(st.ModTime()) > maxAge {
			_ = os.Remove(fn)
		}
	}
}

// Samples returns a copy of samples with from <= T <= to (0 = unbounded).
func (r *Recorder) Samples(from, to int64) []RecSample {
	r.mu.RLock()
	defer r.mu.RUnlock()
	i := sort.Search(len(r.ring), func(i int) bool { return from == 0 || r.ring[i].T >= from })
	j := len(r.ring)
	if to > 0 {
		j = sort.Search(len(r.ring), func(i int) bool { return r.ring[i].T > to })
	}
	if i > j {
		i = j
	}
	return append([]RecSample(nil), r.ring[i:j]...)
}

// Oldest returns the time of the oldest sample held.
func (r *Recorder) Oldest() (int64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.ring) == 0 {
		return 0, false
	}
	return r.ring[0].T, true
}

// Len reports how many samples are held.
func (r *Recorder) Len() int { r.mu.RLock(); defer r.mu.RUnlock(); return len(r.ring) }

// Nearest returns the sample closest to t.
func (r *Recorder) Nearest(t int64) (RecSample, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.ring) == 0 {
		return RecSample{}, false
	}
	i := sort.Search(len(r.ring), func(i int) bool { return r.ring[i].T >= t })
	switch {
	case i == 0:
		return r.ring[0], true
	case i >= len(r.ring):
		return r.ring[len(r.ring)-1], true
	}
	if t-r.ring[i-1].T <= r.ring[i].T-t {
		return r.ring[i-1], true
	}
	return r.ring[i], true
}

// AddMark bookmarks the timeline.
func (r *Recorder) AddMark(label string, t int64) {
	r.mu.Lock()
	r.marks = append(r.marks, Mark{T: t, Label: label})
	r.mu.Unlock()
}

// Marks returns the bookmarks.
func (r *Recorder) Marks() []Mark {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Mark(nil), r.marks...)
}

// Clear empties the in-memory recording.
func (r *Recorder) Clear() {
	r.mu.Lock()
	r.ring, r.marks = nil, nil
	r.last = 0
	r.mu.Unlock()
}

// ExportCSV writes one row per sample (top processes flattened to a column).
func (r *Recorder) ExportCSV(path string, from, to int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"time", "cpu_pct", "mem_pct", "gpu_pct", "disk_busy_pct", "disk_bytes_per_s", "net_bytes_per_s", "top_processes"})
	for _, s := range r.Samples(from, to) {
		top := ""
		for i, p := range s.Top {
			if i >= 5 {
				break
			}
			if i > 0 {
				top += "; "
			}
			top += fmt.Sprintf("%s(%d) %.1f%%", p.Name, p.PID, p.CPU)
		}
		_ = w.Write([]string{
			time.UnixMilli(s.T).Format("2006-01-02 15:04:05"),
			strconv.FormatFloat(s.CPU, 'f', 1, 64), strconv.FormatFloat(s.Mem, 'f', 1, 64),
			strconv.FormatFloat(s.GPU, 'f', 1, 64), strconv.FormatFloat(s.DiskP, 'f', 1, 64),
			strconv.FormatFloat(s.DiskBps, 'f', 0, 64), strconv.FormatFloat(s.NetBps, 'f', 0, 64), top,
		})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	return f.Close()
}
