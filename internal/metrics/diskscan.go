//go:build windows

package metrics

import (
	"container/heap"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ScanEntry is a folder or file with its size on disk (logical bytes).
type ScanEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Files int64  `json:"files"`
	IsDir bool   `json:"isDir"`
}

// ScanProgress is updated atomically while a scan runs.
type ScanProgress struct {
	Files  atomic.Int64
	Dirs   atomic.Int64
	Bytes  atomic.Int64
	Denied atomic.Int64
}

// ScanResult is the outcome of ScanDir.
type ScanResult struct {
	Root      string
	Total     int64
	Files     int64
	Entries   []ScanEntry // immediate children, largest first
	TopFiles  []ScanEntry // largest files anywhere below Root
	Denied    int64
	Cancelled bool
	Elapsed   time.Duration
	Err       error // root could not be opened; partial child failures are counted in Denied
}

const topFileCount = 100

type fileHeap []ScanEntry

func (h fileHeap) Len() int            { return len(h) }
func (h fileHeap) Less(i, j int) bool  { return h[i].Size < h[j].Size }
func (h fileHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *fileHeap) Push(x interface{}) { *h = append(*h, x.(ScanEntry)) }
func (h *fileHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

type scanner struct {
	ctx  context.Context
	prog *ScanProgress
	mu   sync.Mutex
	top  fileHeap
}

func (s *scanner) offer(e ScanEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.top) < topFileCount {
		heap.Push(&s.top, e)
	} else if e.Size > s.top[0].Size {
		s.top[0] = e
		heap.Fix(&s.top, 0)
	}
}

// walk sums a directory tree. Reparse points (symlinks, junctions) are not
// followed, so a scan cannot loop or double-count another volume.
func (s *scanner) walk(dir string) (size, files int64) {
	if s.ctx.Err() != nil {
		return
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		s.prog.Denied.Add(1)
		return
	}
	s.prog.Dirs.Add(1)
	for _, e := range ents {
		if s.ctx.Err() != nil {
			return
		}
		full := filepath.Join(dir, e.Name())
		t := e.Type()
		switch {
		case t&(fs.ModeSymlink|fs.ModeIrregular) != 0:
			continue
		case e.IsDir():
			sz, f := s.walk(full)
			size += sz
			files += f
		default:
			info, err := e.Info()
			if err != nil {
				continue
			}
			sz := info.Size()
			size += sz
			files++
			s.prog.Files.Add(1)
			s.prog.Bytes.Add(sz)
			if sz > 1<<20 { // only files over 1 MiB can be "largest"
				s.offer(ScanEntry{Name: e.Name(), Path: full, Size: sz})
			}
		}
	}
	return
}

// ScanDir measures root and each of its immediate children.
func ScanDir(ctx context.Context, root string, prog *ScanProgress) (res ScanResult) {
	start := time.Now()
	res = ScanResult{Root: root}
	defer func() { res.Elapsed = time.Since(start); res.Cancelled = ctx.Err() != nil }()
	if ctx.Err() != nil {
		return res
	}
	if prog == nil {
		prog = &ScanProgress{}
	}
	sc := &scanner{ctx: ctx, prog: prog}

	ents, err := os.ReadDir(root)
	if err != nil {
		res.Err = err
		prog.Denied.Add(1)
		res.Denied = prog.Denied.Load()
		return res
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
children:
	for _, e := range ents {
		if ctx.Err() != nil {
			break
		}
		e := e
		full := filepath.Join(root, e.Name())
		if e.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			continue
		}
		if !e.IsDir() {
			info, err := e.Info()
			if err != nil {
				continue
			}
			mu.Lock()
			res.Entries = append(res.Entries, ScanEntry{Name: e.Name(), Path: full, Size: info.Size(), Files: 1})
			mu.Unlock()
			prog.Files.Add(1)
			prog.Bytes.Add(info.Size())
			if info.Size() > 1<<20 {
				sc.offer(ScanEntry{Name: e.Name(), Path: full, Size: info.Size()})
			}
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break children
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			sz, f := sc.walk(full)
			mu.Lock()
			res.Entries = append(res.Entries, ScanEntry{Name: e.Name(), Path: full, Size: sz, Files: f, IsDir: true})
			mu.Unlock()
		}()
	}
	wg.Wait()

	for _, e := range res.Entries {
		res.Total += e.Size
		res.Files += e.Files
	}
	sort.Slice(res.Entries, func(i, j int) bool { return res.Entries[i].Size > res.Entries[j].Size })
	res.TopFiles = append([]ScanEntry(nil), sc.top...)
	sort.Slice(res.TopFiles, func(i, j int) bool { return res.TopFiles[i].Size > res.TopFiles[j].Size })
	res.Denied = prog.Denied.Load()
	res.Cancelled = ctx.Err() != nil
	res.Elapsed = time.Since(start)
	return res
}
