//go:build windows

package metrics

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modPdh                   = windows.NewLazySystemDLL("pdh.dll")
	procPdhOpenQuery         = modPdh.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounter = modPdh.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData  = modPdh.NewProc("PdhCollectQueryData")
	procPdhGetFormattedValue = modPdh.NewProc("PdhGetFormattedCounterValue")
	procPdhGetFormattedArray = modPdh.NewProc("PdhGetFormattedCounterArrayW")
	procPdhCloseQuery        = modPdh.NewProc("PdhCloseQuery")
)

const (
	pdhFmtDouble   = 0x00000200
	pdhMoreData    = 0x800007D2
	pdhCStatusOK   = 0x00000000
	pdhCStatusNew  = 0x00000001
	pdhItemSize    = 24 // PDH_FMT_COUNTERVALUE_ITEM_W on x64: name ptr + 16-byte value
	pdhValueOffset = 8
)

// pdhQuery is a thin wrapper around a PDH query handle. Rate counters need two
// collections before they return data, so the first Collect after Open yields
// "no data" values; callers treat that as zero.
type pdhQuery struct{ h uintptr }

// pdhCounter keeps its formatted-array buffer, its result slice and its
// instance names between collections: the instance set barely changes from
// one tick to the next, so steady-state reads allocate nothing.
type pdhCounter struct {
	h     uintptr
	buf   []byte                // grow-only PdhGetFormattedCounterArray buffer
	out   []PDHInstanceValue    // reused result: valid until the next values call
	names map[string]*pdhIntern // keyed by the raw UTF-16LE name bytes
	calls uint32
}

// pdhIntern is one decoded instance name and the collection it was last seen in.
type pdhIntern struct {
	s    string
	seen uint32
}

// pdhInternSweepEvery bounds the interning table: names absent from the most
// recent collection are dropped every this many collections.
const pdhInternSweepEvery = 64

type PDHInstanceValue struct {
	Instance string
	Value    float64
}

func openPdh() (*pdhQuery, error) {
	var h uintptr
	r, _, _ := procPdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&h)))
	if r != 0 {
		return nil, fmt.Errorf("PdhOpenQuery: 0x%08X", uint32(r))
	}
	return &pdhQuery{h: h}, nil
}

func (q *pdhQuery) add(path string) (*pdhCounter, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	var c uintptr
	r, _, _ := procPdhAddEnglishCounter.Call(q.h, uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(&c)))
	if r != 0 {
		return nil, fmt.Errorf("PdhAddEnglishCounter(%s): 0x%08X", path, uint32(r))
	}
	return &pdhCounter{h: c}, nil
}

func (q *pdhQuery) collect() bool {
	r, _, _ := procPdhCollectQueryData.Call(q.h)
	return r == 0
}

func (q *pdhQuery) close() {
	if q != nil && q.h != 0 {
		procPdhCloseQuery.Call(q.h)
		q.h = 0
	}
}

// value reads a single-instance counter as a float.
func (c *pdhCounter) value() (float64, bool) {
	if c == nil {
		return 0, false
	}
	var out struct {
		CStatus uint32
		_       uint32
		Val     float64
	}
	r, _, _ := procPdhGetFormattedValue.Call(c.h, pdhFmtDouble, 0, uintptr(unsafe.Pointer(&out)))
	if r != 0 || (out.CStatus != pdhCStatusOK && out.CStatus != pdhCStatusNew) {
		return 0, false
	}
	return out.Val, true
}

// values reads a wildcard counter (e.g. "\GPU Engine(*)\Utilization Percentage").
// The counter's previous buffer is tried first; PDH_MORE_DATA (the instance
// set grew, or this is the first read) resizes it and retries a bounded
// number of times instead of dropping the whole collection for one poll.
//
// The returned slice is reused by the next call on the same counter: callers
// consume it within the sample and never retain it. Instance strings are
// interned and immutable, so retaining those is fine.
func (c *pdhCounter) values() []PDHInstanceValue {
	if c == nil {
		return nil
	}
	var size, count uint32
	filled := false
	for attempt := 0; attempt < 4; attempt++ {
		size = uint32(len(c.buf))
		var p uintptr
		if size > 0 {
			p = uintptr(unsafe.Pointer(&c.buf[0]))
		}
		r, _, _ := procPdhGetFormattedArray.Call(c.h, pdhFmtDouble,
			uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), p)
		if r == 0 {
			filled = true
			break
		}
		if uint32(r) != pdhMoreData || size == 0 {
			return nil
		}
		// A partially written buffer is not trusted: grow (with headroom, so a
		// process starting on the GPU does not force a second round trip next
		// tick) and fill again.
		c.buf = make([]byte, int(size)+int(size)/4+512)
	}
	if !filled || len(c.buf) == 0 {
		return nil
	}
	buf := c.buf // names point inside it; the item array is bounded by count
	c.calls++
	if c.names == nil {
		c.names = make(map[string]*pdhIntern, count)
	}
	base := uintptr(unsafe.Pointer(&buf[0]))
	out := c.out[:0]
	for i := uint32(0); i < count; i++ {
		if int(i)*pdhItemSize+pdhItemSize > len(buf) {
			break // more instances reported than the buffer holds: stop at the end
		}
		item := buf[i*pdhItemSize:]
		namePtr := *(*uintptr)(unsafe.Pointer(&item[0]))
		status := *(*uint32)(unsafe.Pointer(&item[pdhValueOffset]))
		val := *(*float64)(unsafe.Pointer(&item[pdhValueOffset+8]))
		if status != pdhCStatusOK && status != pdhCStatusNew {
			continue
		}
		// The name string is stored inside buf, after the item array.
		var name string
		if namePtr >= base && namePtr < base+uintptr(len(buf)) {
			name = c.intern(buf, int(namePtr-base))
		}
		out = append(out, PDHInstanceValue{Instance: name, Value: val})
	}
	c.out = out
	c.sweep()
	return out
}

// sweep forgets, every pdhInternSweepEvery collections, the names that were
// not part of the latest one (exited processes, removed devices).
func (c *pdhCounter) sweep() {
	if c.calls%pdhInternSweepEvery != 0 {
		return
	}
	for k, e := range c.names {
		if e.seen != c.calls {
			delete(c.names, k)
		}
	}
}

// intern returns the decoded NUL-terminated UTF-16LE name at off. The lookup
// by raw bytes does not allocate; only a name not seen before is decoded.
func (c *pdhCounter) intern(b []byte, off int) string {
	if off < 0 || off >= len(b) {
		return ""
	}
	end := off
	for end+1 < len(b) && (b[end] != 0 || b[end+1] != 0) {
		end += 2
	}
	if e := c.names[string(b[off:end])]; e != nil {
		e.seen = c.calls
		return e.s
	}
	s := utf16At(b, off)
	c.names[string(b[off:end])] = &pdhIntern{s: s, seen: c.calls}
	return s
}

// utf16At decodes a NUL-terminated UTF-16LE string starting at off in b.
func utf16At(b []byte, off int) string {
	if off < 0 || off >= len(b) {
		return ""
	}
	end, ascii := off, true
	for end+1 < len(b) {
		c := uint16(b[end]) | uint16(b[end+1])<<8
		if c == 0 {
			break
		}
		ascii = ascii && c < 128
		end += 2
	}
	if ascii {
		out := make([]byte, (end-off)/2)
		for i := range out {
			out[i] = b[off+2*i]
		}
		return unsafe.String(unsafe.SliceData(out), len(out)) // out is never written again
	}
	u := make([]uint16, (end-off)/2)
	for i := off; i < end; i += 2 {
		c := uint16(b[i]) | uint16(b[i+1])<<8
		u[(i-off)/2] = c
	}
	return windows.UTF16ToString(u)
}
