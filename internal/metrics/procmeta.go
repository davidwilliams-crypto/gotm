//go:build windows

package metrics

import (
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

type procMeta struct{ path, sid string }

type sidAccountEntry struct {
	name    string
	until   time.Time
	pending bool
}

// Account names belong to SIDs, not processes. Keep completed lookups bounded
// and briefly cache failures. Neither a miss nor a full work queue waits for
// domain resolution. The two workers may block in Windows, but cannot block
// sampling or grow into an unbounded number of goroutines.
type sidAccountCache struct {
	mu      sync.Mutex
	entries map[string]*sidAccountEntry
	limit   int
	jobs    chan string
}

func newSIDAccountCache(limit int, resolve func(string) (string, bool)) *sidAccountCache {
	if limit < 1 {
		limit = 1
	}
	c := &sidAccountCache{entries: make(map[string]*sidAccountEntry), limit: limit, jobs: make(chan string, 64)}
	for i := 0; i < 2; i++ {
		go func() {
			for sid := range c.jobs {
				name, ok := resolve(sid) // owned SID string; no process/token handle escapes
				ttl := 5 * time.Minute
				if !ok {
					name = ""
					ttl = 30 * time.Second
				}
				c.mu.Lock()
				c.entries[sid] = &sidAccountEntry{name: name, until: time.Now().Add(ttl)}
				c.mu.Unlock()
			}
		}()
	}
	return c
}

var processAccounts = newSIDAccountCache(256, resolveSIDAccount)

func (c *sidAccountCache) lookup(sid string) string {
	if sid == "" {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, exists := c.entries[sid]
	if exists && (e.pending || time.Now().Before(e.until)) {
		if e.name != "" {
			return e.name
		}
		return sid
	}
	// Preserve completed cache entries under queue pressure. Producers hold
	// mu, so a slot can only become more available while this check runs.
	if len(c.jobs) == cap(c.jobs) {
		return sid
	}
	if !exists && len(c.entries) >= c.limit {
		var oldest string
		var until time.Time
		for key, e := range c.entries {
			if !e.pending && (oldest == "" || e.until.Before(until)) {
				oldest, until = key, e.until
			}
		}
		if oldest == "" {
			return sid
		} // all slots are queued/in flight; retry next tick
		delete(c.entries, oldest)
	}
	select {
	case c.jobs <- sid:
		c.entries[sid] = &sidAccountEntry{pending: true}
	default:
		// No pending entry for dropped work: a later sample may retry it.
		if exists {
			delete(c.entries, sid)
		}
	}
	return sid
}

func resolveSIDAccount(sidText string) (string, bool) {
	sid, err := windows.StringToSid(sidText)
	if err != nil {
		return "", false
	}
	account, domain, _, err := sid.LookupAccount("")
	if err != nil {
		return "", false
	}
	if domain != "" {
		return domain + "\\" + account, true
	}
	return account, true
}

// metaCache retains path and owned SID per PID + creation time. Owner names
// are read from the shared asynchronous cache each tick, so pending names and
// retries become visible without reopening the process or confusing PID reuse.
type metaCache struct {
	mu       sync.Mutex
	m        map[procKey]procMeta
	accounts *sidAccountCache
}

func newMetaCache() *metaCache {
	return &metaCache{m: map[procKey]procMeta{}, accounts: processAccounts}
}

func (c *metaCache) lookup(k procKey) (path, user string) {
	c.mu.Lock()
	if v, ok := c.m[k]; ok {
		c.mu.Unlock()
		return v.path, c.accounts.lookup(v.sid)
	}
	c.mu.Unlock()
	v := resolveMeta(k.pid)
	c.mu.Lock()
	c.m[k] = v
	c.mu.Unlock()
	return v.path, c.accounts.lookup(v.sid)
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
			// String() copies the SID before token data/handles go out of scope.
			m.sid = tu.User.Sid.String()
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
