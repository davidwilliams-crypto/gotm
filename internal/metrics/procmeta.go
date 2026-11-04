//go:build windows

package metrics

import (
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

type procMeta struct{ path, user string }

type sidAccountEntry struct {
	name  string
	until time.Time
}

// Account names belong to SIDs, not processes. Keep completed lookups bounded
// and briefly cache failures so an unreachable domain is not contacted once
// for every process in the first sample. A miss never holds the hit-path lock.
type sidAccountCache struct {
	mu        sync.Mutex
	resolveMu sync.Mutex
	entries   map[string]sidAccountEntry
	limit     int
}

func newSIDAccountCache(limit int) *sidAccountCache {
	if limit < 1 {
		limit = 1
	}
	return &sidAccountCache{entries: make(map[string]sidAccountEntry), limit: limit}
}

var processAccounts = newSIDAccountCache(256)

func (c *sidAccountCache) cached(sid string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[sid]
	return e.name, ok && time.Now().Before(e.until)
}

func (c *sidAccountCache) lookup(sid string, resolve func() (string, bool)) string {
	if sid == "" {
		name, _ := resolve()
		return name
	}
	if name, ok := c.cached(sid); ok {
		return name
	}
	// Serialize misses, then recheck: simultaneous processes with the same
	// owner share one domain lookup without creating per-process goroutines.
	c.resolveMu.Lock()
	defer c.resolveMu.Unlock()
	if name, ok := c.cached(sid); ok {
		return name
	}
	name, ok := resolve()
	ttl := 5 * time.Minute
	if !ok {
		ttl = 30 * time.Second
	}
	entry := sidAccountEntry{name: name, until: time.Now().Add(ttl)}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[sid]; !exists && len(c.entries) >= c.limit {
		var oldest string
		var until time.Time
		for key, e := range c.entries {
			if oldest == "" || e.until.Before(until) {
				oldest, until = key, e.until
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[sid] = entry
	return name
}

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
	m.path = processImagePath(h)

	var tok windows.Token
	if windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok) == nil {
		defer tok.Close()
		if tu, err := tok.GetTokenUser(); err == nil {
			m.user = processAccounts.lookup(tu.User.Sid.String(), func() (string, bool) {
				account, domain, _, err := tu.User.Sid.LookupAccount("")
				if err != nil {
					return "", false
				}
				if domain != "" {
					return domain + "\\" + account, true
				}
				return account, true
			})
		}
	}
	return m
}

// ProcessPath returns the image path of a live process ("" when inaccessible).
func ProcessPath(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	return processImagePath(h)
}

func processImagePath(h windows.Handle) string {
	buf := make([]uint16, 1024)
	n := uint32(len(buf))
	if windows.QueryFullProcessImageName(h, 0, &buf[0], &n) != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}
