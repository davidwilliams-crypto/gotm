//go:build windows

package metrics

import (
	"bufio"
	"bytes"
	"context"
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
	T              int64     `json:"t"`
	CPU            float64   `json:"cpu"`
	Mem            float64   `json:"mem"`
	GPU            float64   `json:"gpu"`
	GPUUnavailable bool      `json:"gpuUnavailable,omitempty"`
	DiskP          float64   `json:"diskP"`
	DiskBps        float64   `json:"diskBps"`
	NetBps         float64   `json:"netBps"`
	Top            []RecProc `json:"top"`
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

	// A single worker owns the file. No filesystem operation holds mu.
	file                                     io.WriteCloser
	fileDay                                  string
	diskErr                                  error // last persistence failure; cleared by the next good write
	diskQueue                                []RecSample
	diskHead, diskCount                      int
	diskWake                                 chan struct{}
	diskDone                                 chan struct{}
	diskProgress                             chan struct{}
	diskRunning, closed                      bool
	diskAccepted, diskCompleted, diskDropped uint64
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if v && !r.persist {
		r.diskDropped = 0
		r.diskErr = nil
	}
	r.persist = v
	r.wakeDiskLocked()
}

// DiskErr reports the last on-disk persistence failure (disk full, locked
// file, redirected folder while elevated), or nil when writes are succeeding.
func (r *Recorder) DiskErr() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.diskDropped > 0 {
		return fmt.Errorf("disk backlog full: %d samples kept only in memory", r.diskDropped)
	}
	return r.diskErr
}

func (r *Recorder) Persist() bool { r.mu.RLock(); defer r.mu.RUnlock(); return r.persist }

// Add records a snapshot if enabled and the interval has elapsed.
func (r *Recorder) Add(s *Snapshot) {
	if s == nil {
		return
	}
	r.mu.Lock()
	if !r.enabled || r.closed {
		r.mu.Unlock()
		return
	}
	if s.T < r.last {
		// Range and nearest-sample searches require chronological, unique times.
		// Supersede the old future tail (including any sample at this time),
		// just as the sampler does for its graph history after a clock change.
		i := sort.Search(len(r.ring), func(i int) bool { return r.ring[i].T >= s.T })
		clear(r.ring[i:])
		r.ring = r.ring[:i]
		r.last = 0
	}
	if s.T-r.last < r.every.Milliseconds() {
		r.mu.Unlock()
		return
	}
	r.last = s.T

	rs := RecSample{T: s.T, CPU: s.CPU, Mem: round1(s.Mem.Percent), GPU: s.GPU.Percent,
		GPUUnavailable: !s.GPU.OK,
		DiskP:          s.Disk.Percent, DiskBps: s.Disk.ReadBps + s.Disk.WriteBps, NetBps: s.Net.RxBps + s.Net.TxBps}
	rs.Top = topProcs(s.Procs)

	r.ring = append(r.ring, rs)
	if len(r.ring) > r.capacity {
		n := copy(r.ring, r.ring[len(r.ring)-r.capacity:])
		clear(r.ring[n:])
		r.ring = r.ring[:n]
	}
	if r.persist && r.dir != "" {
		r.enqueueDiskLocked(rs)
	}
	r.mu.Unlock()
}

// The queue covers up to 256 pending samples (over eight minutes at the
// default interval). A permanently stalled disk cannot grow memory without
// bound. Rejected disk samples remain in the normal in-memory recording and
// DiskErr reports the gap until persistence is explicitly restarted.
func (r *Recorder) enqueueDiskLocked(s RecSample) {
	if r.diskQueue == nil {
		r.diskQueue = make([]RecSample, min(r.capacity, 256))
	}
	if r.diskCount == len(r.diskQueue) {
		r.diskDropped++
		return
	}
	r.diskQueue[(r.diskHead+r.diskCount)%len(r.diskQueue)] = s
	r.diskCount++
	r.diskAccepted++
	if !r.diskRunning {
		r.diskRunning = true
		r.diskWake = make(chan struct{}, 1)
		r.diskDone = make(chan struct{})
		r.diskProgress = make(chan struct{})
		go r.runDisk()
	}
	r.wakeDiskLocked()
}

func (r *Recorder) wakeDiskLocked() {
	if r.diskRunning {
		select {
		case r.diskWake <- struct{}{}:
		default:
		}
	}
}

func (r *Recorder) runDisk() {
	for {
		r.mu.Lock()
		if r.diskCount > 0 {
			s := r.diskQueue[r.diskHead]
			r.diskQueue[r.diskHead] = RecSample{}
			r.diskHead = (r.diskHead + 1) % len(r.diskQueue)
			r.diskCount--
			r.mu.Unlock()
			err := r.appendDisk(s)
			r.mu.Lock()
			r.diskErr = err
			r.diskCompleted++
			close(r.diskProgress)
			r.diskProgress = make(chan struct{})
			r.mu.Unlock()
			continue
		}
		if !r.persist {
			r.mu.Unlock()
			err := r.closeDisk()
			r.mu.Lock()
			if err != nil {
				r.diskErr = err
			}
			// An enable/Add may have arrived while Close was blocked.
			if r.diskCount > 0 || r.persist {
				r.mu.Unlock()
				continue
			}
			r.diskRunning = false
			r.diskQueue = nil
			r.diskHead = 0
			close(r.diskDone)
			r.mu.Unlock()
			return
		}
		wake := r.diskWake
		r.mu.Unlock()
		<-wake
	}
}

// Flush waits for disk attempts accepted before the call. Call this from a
// background or shutdown path with a deadline, never from the UI thread.
// DiskErr separately reports write failures or a full backlog.
func (r *Recorder) Flush(ctx context.Context) error {
	r.mu.RLock()
	target := r.diskAccepted
	for r.diskCompleted < target {
		progress := r.diskProgress
		r.mu.RUnlock()
		select {
		case <-progress:
		case <-ctx.Done():
			return ctx.Err()
		}
		r.mu.RLock()
	}
	r.mu.RUnlock()
	return nil
}

// Close stops capture and drains accepted writes before closing the file.
// A timeout releases the caller; the one worker still owns any blocked I/O.
func (r *Recorder) Close(ctx context.Context) error {
	r.mu.Lock()
	r.closed, r.persist = true, false
	r.wakeDiskLocked()
	done, running := r.diskDone, r.diskRunning
	r.mu.Unlock()
	if !running {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Only thirteen leaders can be retained. Fixed-size ranked selections avoid
// sorting and allocating an index for every process in a busy system.
func topProcs(procs []Proc) []RecProc {
	var cpu [10]int
	var mem [3]int
	nc, nm := 0, 0
	insert := func(top []int, n *int, idx int, better func(int, int) bool) {
		at := 0
		for at < *n && !better(idx, top[at]) {
			at++
		}
		if at == len(top) {
			return
		}
		if *n < len(top) {
			*n++
		}
		copy(top[at+1:*n], top[at:*n-1])
		top[at] = idx
	}
	for i := range procs {
		if procs[i].PID == 0 {
			continue
		}
		insert(cpu[:], &nc, i, func(a, b int) bool { return procs[a].CPU > procs[b].CPU })
		insert(mem[:], &nm, i, func(a, b int) bool { return procs[a].Mem > procs[b].Mem })
	}
	out := make([]RecProc, 0, nc+nm)
	add := func(idx int) {
		p := procs[idx]
		out = append(out, RecProc{PID: p.PID, Name: p.Name, CPU: p.CPU, Mem: p.Mem})
	}
	for _, idx := range cpu[:nc] {
		add(idx)
	}
	for _, idx := range mem[:nm] {
		found := false
		for _, c := range cpu[:nc] {
			if c == idx {
				found = true
				break
			}
		}
		if !found {
			add(idx)
		}
	}
	// Small stable insertion sort avoids a reflection closure allocation.
	for i := 1; i < len(out); i++ {
		v := out[i]
		j := i
		for j > 0 && out[j-1].CPU < v.CPU {
			out[j] = out[j-1]
			j--
		}
		out[j] = v
	}
	return out
}

// errRedirectedPath is reported when persistence is on but the recordings
// folder (or the file) is a link and gotm is elevated: writing through it
// could aim an administrator's write at a target of someone else's choosing.
var errRedirectedPath = errors.New("the recordings folder or file is a link; gotm is elevated and will not write through it")

func (r *Recorder) appendDisk(rs RecSample) error {
	day := time.UnixMilli(rs.T).Format("2006-01-02")
	if r.file == nil || r.fileDay != day {
		if r.file != nil {
			if err := r.closeDisk(); err != nil {
				return err
			}
		}
		path := filepath.Join(r.dir, day+".jsonl")
		if !safepath.WritableFile(path) {
			return errRedirectedPath
		}
		if err := os.MkdirAll(r.dir, 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		r.file, r.fileDay = f, day
	}
	b, err := json.Marshal(rs)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	n, err := r.file.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err != nil {
		// A failing write must not pass silently: drop the handle so the next
		// sample reopens (recovering from a locked or replaced file) and
		// report the error until one write succeeds.
		_ = r.closeDisk()
		return err
	}
	return nil
}

func (r *Recorder) closeDisk() error {
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file, r.fileDay = nil, ""
	return err
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
	return ExportSamplesCSV(path, r.Samples(from, to))
}

// ExportSamplesCSV exports a previously captured window. Callers can freeze
// the recording before starting background I/O, so Clear or new samples do
// not change an export while its destination is slow to open.
func ExportSamplesCSV(path string, samples []RecSample) error {
	if !safepath.WritableFile(path) {
		return errRedirectedPath
	}
	// A bounded shutdown can end an in-flight export. Do not truncate an
	// existing destination until the new CSV has been written and closed.
	f, err := os.CreateTemp(filepath.Dir(path), ".gotm-export-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"time", "cpu_pct", "mem_pct", "gpu_pct", "disk_busy_pct", "disk_bytes_per_s", "net_bytes_per_s", "top_processes"})
	for _, s := range samples {
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
			recordedGPUCSV(s), strconv.FormatFloat(s.DiskP, 'f', 1, 64),
			strconv.FormatFloat(s.DiskBps, 'f', 0, 64), strconv.FormatFloat(s.NetBps, 'f', 0, 64), csvCell(top),
		})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if !safepath.WritableFile(path) {
		return errRedirectedPath
	}
	return os.Rename(f.Name(), path)
}

func recordedGPUCSV(s RecSample) string {
	if s.GPUUnavailable {
		return ""
	}
	return strconv.FormatFloat(s.GPU, 'f', 1, 64)
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
