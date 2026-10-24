//go:build windows

package metrics

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

type procMeta struct{ path, user string }

// metaCache resolves slow-changing per-process facts (image path, owner) once
// per process instance (PID + creation time) instead of every tick.
type metaCache struct {
	mu sync.Mutex
	m  map[procKey]procMeta
}

func newMetaCache() *metaCache { return &metaCache{m: map[procKey]procMeta{}} }

func (c *metaCache) lookup(k procKey) (path, user string) {
	c.mu.Lock()
	if v, ok := c.m[k]; ok {
		c.mu.Unlock()
		return v.path, v.user
	}
	c.mu.Unlock()
	v := resolveMeta(k.pid)
	c.mu.Lock()
	c.m[k] = v
	c.mu.Unlock()
	return v.path, v.user
}

func (c *metaCache) prune(live map[procKey]struct{}) {
	c.mu.Lock()
	for k := range c.m {
		if _, ok := live[k]; !ok {
			delete(c.m, k)
		}
	}
	c.mu.Unlock()
}

func resolveMeta(pid uint32) procMeta {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return procMeta{}
	}
	defer windows.CloseHandle(h)

	var m procMeta
	buf := make([]uint16, 1024)
	n := uint32(len(buf))
	if windows.QueryFullProcessImageName(h, 0, &buf[0], &n) == nil {
		m.path = windows.UTF16ToString(buf[:n])
	}

	var tok windows.Token
	if windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok) == nil {
		defer tok.Close()
		if tu, err := tok.GetTokenUser(); err == nil {
			if account, domain, _, err := tu.User.Sid.LookupAccount(""); err == nil {
				if domain != "" {
					m.user = domain + "\\" + account
				} else {
					m.user = account
				}
			}
		}
	}
	return m
}

// ProcessPath returns the image path of a live process ("" when inaccessible).
func ProcessPath(pid uint32) string { return resolveMeta(pid).path }

// compile-time guard: unsafe is used by sibling files in this package.
var _ = unsafe.Sizeof(0)
