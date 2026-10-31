//go:build windows

package metrics

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"gotm/internal/safepath"
	"io"
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

	// fileMu serializes disk writes and is held INSTEAD of mu during file I/O,
	// so chart reads (Samples, Nearest, Len) never wait on the file system.
	fileMu  sync.Mutex
	file    *os.File
	fileDay string
	diskErr error // last persistence failure; cleared by the next good write
}

// NewRecorder keeps `retention` of history, one sample per `every`. Capture
// starts disabled; each app session requires an explicit SetEnabled(true).
func NewRecorder(retention, every time.Duration, dir string) *Recorder {
	if every < time.Second {
		every = time.Second
	}
	n := int(retention / every)
	if n < 60 {
		n = 60
	}
	return &Recorder{capacity: n, every: every, dir: dir}
}

func (r *Recorder) SetEnabled(v bool) { r.mu.Lock(); r.enabled = v; r.mu.Unlock() }
func (r *Recorder) Enabled() bool     { r.mu.RLock(); defer r.mu.RUnlock(); return r.enabled }

// SetPersist turns on appending samples to <dir>\yyyy-mm-dd.jsonl.
func (r *Recorder) SetPersist(v bool) {
	// fileMu before r.mu, the same order appendDisk uses: an in-flight
	// appendDisk then sees the switch-off and stops without reopening.
	r.fileMu.Lock()
	defer r.fileMu.Unlock()
	r.mu.Lock()
	r.persist = v
	r.mu.Unlock()
	if !v {
		if r.file != nil {
			_ = r.file.Close()
			r.file, r.fileDay = nil, ""
		}
		r.diskErr = nil
	}
}

// DiskErr reports the last on-disk persistence failure (disk full, locked
// file, redirected folder while elevated), or nil when writes are succeeding.
func (r *Recorder) DiskErr() error {
	r.fileMu.Lock()
	defer r.fileMu.Unlock()
	return r.diskErr
}
func (r *Recorder) Persist() bool { r.mu.RLock(); defer r.mu.RUnlock(); return r.persist }

// Add records a snapshot if enabled and the interval has elapsed.
func (r *Recorder) Add(s *Snapshot) {
	if s == nil {
		return
	}
	r.mu.Lock()
	if !r.enabled {
		r.mu.Unlock()
		return
	}
	if s.T < r.last {
		// Range and nearest-sample searches require chronological, unique times.
		// Supersede the old future tail (including any sample at this time),
		// just as the sampler does for its graph history after a clock change.
		i := sort.Search(len(r.ring), func(i int) bool { return r.ring[i].T >= s.T })
		r.ring = r.ring[:i]
		r.last = 0
	}
	if s.T-r.last < r.every.Milliseconds() {
		r.mu.Unlock()
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
	persist := r.persist
	r.mu.Unlock()
	// The disk write happens without r.mu: readers of the ring never stall
	// behind a slow or networked recordings folder.
	if persist {
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

// errRedirectedPath is reported when persistence is on but the recordings
// folder (or the file) is a link and gotm is elevated: writing through it
// could aim an administrator's write at a target of someone else's choosing.
var errRedirectedPath = errors.New("the recordings folder or file is a link; gotm is elevated and will not write through it")

func (r *Recorder) appendDisk(rs RecSample) {
	if r.dir == "" {
		return
	}
	r.fileMu.Lock()
	defer r.fileMu.Unlock()
	r.mu.RLock()
	persist := r.persist
	r.mu.RUnlock()
	if !persist {
		return // switched off after Add snapshotted it: no trailing write
	}
	day := time.UnixMilli(rs.T).Format("2006-01-02")
	if r.file == nil || r.fileDay != day {
		if r.file != nil {
			_ = r.file.Close()
			r.file, r.fileDay = nil, ""
		}
		path := filepath.Join(r.dir, day+".jsonl")
		if !safepath.WritableFile(path) {
			r.diskErr = errRedirectedPath
			return
		}
		_ = os.MkdirAll(r.dir, 0o700)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			r.diskErr = err
			return
		}
		r.file, r.fileDay = f, day
	}
	b, err := json.Marshal(rs)
	if err != nil {
		r.diskErr = err
		return
	}
	if _, err := r.file.Write(append(b, '\n')); err != nil {
		// A failing write must not pass silently: drop the handle so the next
		// sample reopens (recovering from a locked or replaced file) and
		// report the error until one write succeeds.
		r.diskErr = err
		_ = r.file.Close()
		r.file, r.fileDay = nil, ""
		return
	}
	r.diskErr = nil
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
		loaded = appendLines(loaded, f, cut, now)
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

// appendLines parses one newline-delimited JSON recording file into dst. A
// line that is corrupt or longer than 1 MiB is skipped; the rest of the file
// still loads — one damaged line must not silently discard the samples after
// it (a bufio.Scanner would stop the whole file at its first long line).
func appendLines(dst []RecSample, f io.Reader, cut, now int64) []RecSample {
	rd := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := rd.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			for err == bufio.ErrBufferFull { // discard the oversized line's remainder
				_, err = rd.ReadSlice('\n')
			}
			if err != nil {
				return dst
			}
			continue
		}
		if len(line) > 0 {
			var rs RecSample
			if json.Unmarshal(bytes.TrimRight(line, "\r\n"), &rs) == nil && rs.T >= cut && rs.T <= now {
				dst = append(dst, rs)
			}
		}
		if err != nil { // io.EOF or a real read error: this file is done
			return dst
		}
	}
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
// The chosen path gets the same elevated-link refusal as the on-disk history,
// and the process-name column is neutralized so spreadsheets open it as text
// instead of evaluating a name that begins with =, +, - or @ as a formula.
func (r *Recorder) ExportCSV(path string, from, to int64) error {
	if !safepath.WritableFile(path) {
		return errRedirectedPath
	}
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
			strconv.FormatFloat(s.DiskBps, 'f', 0, 64), strconv.FormatFloat(s.NetBps, 'f', 0, 64), csvCell(top),
		})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	return f.Close()
}

// csvCell stops spreadsheet formula injection: Windows file names may begin
// with =, +, - or @, and Excel (and friends) evaluate such a cell — including
// DDE — when the export is opened. A leading apostrophe forces text.
func csvCell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}
