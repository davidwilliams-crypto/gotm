//go:build windows

package metrics

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testAccountCache(t *testing.T, limit int, resolve func(string) (string, bool)) *sidAccountCache {
	t.Helper()
	c := newSIDAccountCache(limit, resolve)
	t.Cleanup(func() { close(c.jobs) })
	return c
}

func awaitAccount(t *testing.T, c *sidAccountCache, sid, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := c.lookup(sid); got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("owner %q, want %q", c.lookup(sid), want)
}

func TestSIDAccountCacheSharesLookupAcrossProcesses(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	c := testAccountCache(t, 256, func(sid string) (string, bool) { calls.Add(1); <-release; return `DOMAIN\owner`, true })
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := c.lookup("S-1-5-21-123"); got != "S-1-5-21-123" {
				t.Errorf("pending owner %q", got)
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("lookups waited for resolver")
	}
	close(release)
	awaitAccount(t, c, "S-1-5-21-123", `DOMAIN\owner`)
	if got := calls.Load(); got != 1 {
		t.Fatalf("%d domain lookups for one SID", got)
	}
}

func TestSIDAccountCacheBoundedBackpressure(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	c := testAccountCache(t, 256, func(sid string) (string, bool) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return "owner", true
	})
	c.lookup("first")
	c.lookup("second")
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("workers did not start")
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			sid := fmt.Sprint("sid-", i)
			if got := c.lookup(sid); got != sid {
				t.Errorf("pending owner %q", got)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("full queue blocked sampler")
	}
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n > 66 || len(c.jobs) > 64 {
		close(release)
		t.Fatalf("unbounded pending work: %d cache, %d queue", n, len(c.jobs))
	}
	close(release)
	awaitAccount(t, c, "sid-999", "owner")
}

func TestSIDAccountCacheBoundAndFailureRetry(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	var calls atomic.Int32
	c := testAccountCache(t, 2, func(sid string) (string, bool) {
		calls.Add(1)
		if sid == "failure" && fail.Load() {
			return "", false
		}
		return "owner-" + sid, true
	})
	for _, sid := range []string{"one", "two", "three"} {
		awaitAccount(t, c, sid, "owner-"+sid)
	}
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n != 2 {
		t.Fatalf("cache holds %d entries", n)
	}
	c.lookup("failure")
	deadline := time.Now().Add(3 * time.Second)
	for {
		c.mu.Lock()
		e := c.entries["failure"]
		ready := e != nil && !e.pending
		c.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failure not completed")
		}
		time.Sleep(time.Millisecond)
	}
	before := calls.Load()
	fail.Store(false)
	if got := c.lookup("failure"); got != "failure" {
		t.Fatal(got)
	}
	if calls.Load() != before {
		t.Fatal("failed SID retried before expiry")
	}
	c.mu.Lock()
	c.entries["failure"].until = time.Now().Add(-time.Second)
	c.mu.Unlock()
	awaitAccount(t, c, "failure", "owner-failure")
}

func TestProcessMetadataCacheRefreshesOwnerAndKeepsInstanceIdentity(t *testing.T) {
	release := make(chan struct{})
	accounts := testAccountCache(t, 4, func(sid string) (string, bool) { <-release; return "owner-" + sid, true })
	c := newMetaCache()
	c.accounts = accounts
	old, current := procKey{pid: 7, create: 1}, procKey{pid: 7, create: 2}
	c.m[old] = procMeta{path: "old.exe", sid: "oldSID"}
	c.m[current] = procMeta{path: "new.exe", sid: "newSID"}
	if p, u := c.lookup(current); p != "new.exe" || u != "newSID" {
		close(release)
		t.Fatalf("pending %q %q", p, u)
	}
	close(release)
	awaitAccount(t, accounts, "newSID", "owner-newSID")
	if p, u := c.lookup(current); p != "new.exe" || u != "owner-newSID" {
		t.Fatalf("owner stayed pending %q %q", p, u)
	}
	c.prune(map[procKey]struct{}{current: {}})
	if len(c.m) != 1 || c.m[current].path != "new.exe" {
		t.Fatal("PID-instance pruning changed")
	}
}

func TestSIDAccountSlowDomainDoesNotBlockOtherOwners(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	c := testAccountCache(t, 8, func(sid string) (string, bool) {
		if sid == "slow" {
			close(started)
			<-release
		}
		return "owner-" + sid, true
	})
	c.lookup("slow")
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("resolver did not start")
	}
	defer close(release)
	awaitAccount(t, c, "fast", "owner-fast")
	for i := 0; i < 10; i++ {
		if got := c.lookup("fast"); got != "owner-fast" {
			t.Fatal(got)
		}
	}
}

func TestInaccessibleProcessDoesNotQueueOwnerLookup(t *testing.T) {
	var calls atomic.Int32
	c := newMetaCache()
	c.accounts = testAccountCache(t, 4, func(string) (string, bool) { calls.Add(1); return "wrong", true })
	if path, user := c.lookup(procKey{pid: 0}); path != "" || user != "" {
		t.Fatalf("inaccessible process: %q %q", path, user)
	}
	if calls.Load() != 0 || len(c.accounts.jobs) != 0 {
		t.Fatal("missing SID enqueued account work")
	}
}

func TestProcessPathWithoutOwnerResolution(t *testing.T) {
	processAccounts.mu.Lock()
	done := make(chan string, 1)
	go func() { done <- ProcessPath(uint32(os.Getpid())) }()
	select {
	case got := <-done:
		processAccounts.mu.Unlock()
		if got == "" {
			t.Fatal("current process path unavailable")
		}
	case <-time.After(5 * time.Second):
		processAccounts.mu.Unlock()
		<-done
		t.Fatal("path-only query waited for account resolution")
	}
	if got := ProcessPath(0); got != "" {
		t.Fatalf("inaccessible path %q", got)
	}
}

func BenchmarkSIDAccountCacheSameOwner(b *testing.B) {
	c := newSIDAccountCache(256, func(string) (string, bool) { return `DOMAIN\owner`, true })
	b.Cleanup(func() { close(c.jobs) })
	c.entries["S-1-5-21-123"] = &sidAccountEntry{name: `DOMAIN\owner`, until: time.Now().Add(time.Hour)}
	b.ReportAllocs()
	for b.Loop() {
		c.lookup("S-1-5-21-123")
	}
}
