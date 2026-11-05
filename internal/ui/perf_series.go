//go:build windows

package ui

import (
	"math"

	"gotm/internal/metrics"
)

// historySource is the part of the sampler the Performance page reads.
type historySource interface {
	AppendHistoryAfter(dst []metrics.HistPoint, after int64) ([]metrics.HistPoint, metrics.HistoryInfo)
	History() []metrics.HistPoint
}

// histMirror is the page's copy of the sampler's history, kept up to date
// incrementally: each tick appends only the points the page has not seen and
// drops those the sampler evicted, instead of copying the whole ring (up to
// 14,400 points at the longest history and fastest refresh).
//
// Points are absolutely numbered: view()[0] is point number base. A full
// reload (first use, clock rollback, or anything that does not reconcile)
// starts a new epoch, which tells derived series to start over.
type histMirror struct {
	buf   []metrics.HistPoint // live points are buf[off:]
	off   int
	base  int    // absolute number of buf[off]
	epoch uint64 // 0 = never synchronized
}

func (m *histMirror) view() []metrics.HistPoint { return m.buf[m.off:] }

// sync brings the mirror up to date with src.
func (m *histMirror) sync(src historySource) {
	live := m.view()
	if m.epoch == 0 {
		m.reload(src)
		return
	}
	after := int64(math.MinInt64)
	if len(live) > 0 {
		after = live[len(live)-1].T
	}
	// Reclaim the evicted prefix before the buffer would grow, so appending
	// stays amortized O(new points) without retaining dead points.
	if m.off > 0 && (m.off >= len(live) || len(m.buf) == cap(m.buf)) {
		n := copy(m.buf, live)
		clear(m.buf[n:])
		m.buf, m.off = m.buf[:n], 0
	}
	var info metrics.HistoryInfo
	m.buf, info = src.AppendHistoryAfter(m.buf, after)
	if info.Len == 0 {
		m.dropFront(len(m.buf) - m.off)
		return
	}
	// After a clock rollback the sampler drops the points that now lie in
	// the future, so this page's newest point is no longer in the ring
	// (unless every older point was evicted, which the checks below cover).
	if len(live) > 0 && after >= info.Oldest && info.Prev != after {
		m.reload(src)
		return
	}
	live = m.view()
	k := 0
	for k < len(live) && live[k].T < info.Oldest {
		k++
	}
	m.dropFront(k)
	live = m.view()
	if len(live) != info.Len || live[0].T != info.Oldest || live[len(live)-1].T != info.Newest {
		m.reload(src) // e.g. a point with a repeated timestamp: start over
	}
}

func (m *histMirror) dropFront(k int) {
	if k <= 0 {
		return
	}
	clear(m.buf[m.off : m.off+k]) // release the evicted points' slices
	m.off += k
	m.base += k
	if m.off == len(m.buf) {
		m.buf, m.off = m.buf[:0], 0
	}
}

func (m *histMirror) reload(src historySource) {
	clear(m.buf)
	m.buf, m.off, m.base = src.History(), 0, 0
	m.epoch++
}

// derivedSeries is one curve extracted from the mirrored history (CPU, one
// core, one GPU engine...). It is updated by appending the new points and
// dropping the evicted ones, so a tick costs O(new points) per curve rather
// than a fresh allocation and a full pass for every graph on every paint.
type derivedSeries struct {
	epoch uint64
	base  int // absolute number of vals[off]
	buf   []float64
	off   int
	used  uint64 // paint that last used it
}

// seriesKey names a derived curve.
type seriesKey struct {
	kind int
	idx  int
	id   string // GPU adapter
	name string // GPU engine
}

const (
	serCPU = iota
	serMem
	serGPU    // legacy combined GPU
	serGPUAdp // one adapter's utilization
	serGPUEng // one adapter engine
	serGPUDed
	serGPUShr
	serDiskR
	serDiskW
	serNetRx
	serNetTx
	serCore
)

// derive returns the curve f over the mirrored history and a version that
// changes whenever its content does. The slice is owned by the page and is
// valid until the next derive of the same key.
func (d *derivedSeries) derive(m *histMirror, f func(*metrics.HistPoint) float64) []float64 {
	live := m.view()
	if d.epoch != m.epoch || d.base > m.base || d.base+len(d.buf)-d.off < m.base {
		clear(d.buf)
		d.buf, d.off, d.base, d.epoch = d.buf[:0], 0, m.base, m.epoch
	}
	if k := m.base - d.base; k > 0 {
		d.off += k
		d.base += k
	}
	have := len(d.buf) - d.off
	if have > len(live) {
		d.buf = d.buf[:d.off+len(live)]
		have = len(live)
	}
	if have < len(live) && d.off > 0 && (d.off >= have || len(d.buf) == cap(d.buf)) {
		n := copy(d.buf, d.buf[d.off:])
		d.buf, d.off = d.buf[:n], 0
	}
	for i := have; i < len(live); i++ {
		d.buf = append(d.buf, f(&live[i]))
	}
	return d.buf[d.off:]
}

// version identifies the content derive returned for key: the same epoch,
// first point and length always hold the same values.
func (d *derivedSeries) version(key seriesKey) uint64 {
	h := keyHash(hashWords(uint64(key.kind), uint64(key.idx), d.epoch, uint64(d.base), uint64(len(d.buf)-d.off)))
	for _, s := range []string{key.id, key.name} {
		h.add(uint64(len(s)))
		for i := 0; i < len(s); i++ {
			h.add(uint64(s[i]))
		}
	}
	return uint64(h)
}

// curve returns one derived series of the page's history and its version.
// Without a synchronized mirror (tests that set hist directly), it extracts
// the values from hist and reports version 0, which makes the graph cache
// hash the values instead.
func (p *perfPage) curve(key seriesKey, f func(*metrics.HistPoint) float64) ([]float64, uint64) {
	m := &p.mirror
	if live := m.view(); m.epoch == 0 || len(live) != len(p.hist) || (len(live) > 0 && &live[0] != &p.hist[0]) {
		out := make([]float64, len(p.hist))
		for i := range p.hist {
			out[i] = f(&p.hist[i])
		}
		return out, 0
	}
	if p.curves == nil {
		p.curves = map[seriesKey]*derivedSeries{}
	}
	d := p.curves[key]
	if d == nil {
		d = &derivedSeries{}
		p.curves[key] = d
	}
	d.used = p.paints
	vals := d.derive(m, f)
	return vals, d.version(key)
}

// pruneCurves forgets curves no paint has used for a while (a GPU engine
// that disappeared, a CPU set that shrank, a card no longer shown).
func (p *perfPage) pruneCurves() {
	for k, d := range p.curves {
		if p.paints-d.used > 64 {
			delete(p.curves, k)
		}
	}
}

// syncHistory updates the mirrored history from the sampler.
func (p *perfPage) syncHistory() {
	if p.app == nil || p.app.sampler == nil {
		return
	}
	p.mirror.sync(p.app.sampler)
	p.hist = p.mirror.view()
}

// releaseDerived drops the derived curves while the page is hidden; the
// mirror itself stays, so showing the page again only fetches what is new.
func (p *perfPage) releaseDerived() { p.curves = nil }

// gpuPoint finds one adapter in a history point.
func gpuPoint(h *metrics.HistPoint, id string) *metrics.GPUHist {
	for k := range h.GPUs {
		if h.GPUs[k].ID == id {
			return &h.GPUs[k]
		}
	}
	return nil
}
