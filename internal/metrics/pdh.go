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

type pdhCounter struct{ h uintptr }

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
func (c *pdhCounter) values() []PDHInstanceValue {
	if c == nil {
		return nil
	}
	var size, count uint32
	r, _, _ := procPdhGetFormattedArray.Call(c.h, pdhFmtDouble,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), 0)
	if uint32(r) != pdhMoreData || size == 0 {
		return nil
	}
	buf := make([]byte, size)
	r, _, _ = procPdhGetFormattedArray.Call(c.h, pdhFmtDouble,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)),
		uintptr(unsafe.Pointer(&buf[0])))
	if r != 0 {
		return nil
	}
	base := uintptr(unsafe.Pointer(&buf[0]))
	out := make([]PDHInstanceValue, 0, count)
	for i := uint32(0); i < count; i++ {
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
			off := int(namePtr - base)
			name = utf16At(buf, off)
		}
		out = append(out, PDHInstanceValue{Instance: name, Value: val})
	}
	return out
}

// utf16At decodes a NUL-terminated UTF-16LE string starting at off in b.
func utf16At(b []byte, off int) string {
	var u []uint16
	for i := off; i+1 < len(b); i += 2 {
		c := uint16(b[i]) | uint16(b[i+1])<<8
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return windows.UTF16ToString(append(u, 0))
}
