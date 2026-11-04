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

func TestSIDAccountCacheSharesLookupAcrossProcesses(t *testing.T) {
	c := newSIDAccountCache(256)
	var calls atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := c.lookup("S-1-5-21-123", func() (string, bool) { calls.Add(1); return `DOMAIN\owner`, true }); got != `DOMAIN\owner` {
				t.Errorf("owner %q", got)
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("%d domain lookups for one owner", got)
	}
}

func TestSIDAccountCacheBoundAndExpiry(t *testing.T) {
	c := newSIDAccountCache(2)
	for _, key := range []string{"one", "two", "three"} {
		c.lookup(key, func() (string, bool) { return key, true })
	}
	if len(c.entries) != 2 {
		t.Fatalf("cache holds %d entries", len(c.entries))
	}
	c.lookup("failure", func() (string, bool) { return "", false })
	if got := c.lookup("failure", func() (string, bool) { t.Fatal("negative cache repeated lookup"); return "", false }); got != "" {
		t.Fatal(got)
	}
	c.mu.Lock()
	entry := c.entries["failure"]
	entry.until = time.Now().Add(-time.Second)
	c.entries["failure"] = entry
	c.mu.Unlock()
	if got := c.lookup("failure", func() (string, bool) { return "recovered", true }); got != "recovered" {
		t.Fatalf("expired failure stuck: %q", got)
	}
}

func TestSIDAccountCacheHitDoesNotWaitForOtherDomain(t *testing.T) {
	c := newSIDAccountCache(4)
	c.lookup("known", func() (string, bool) { return "owner", true })
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		c.lookup("slow", func() (string, bool) { close(started); <-release; return "domain", true })
	}()
	<-started
	done := make(chan string, 1)
	go func() { done <- c.lookup("known", func() (string, bool) { return "wrong", true }) }()
	select {
	case got := <-done:
		if got != "owner" {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		close(release)
		<-finished
		t.Fatal("cache hit blocked on domain lookup")
	}
	close(release)
	<-finished
}

func TestProcessPathWithoutOwnerResolution(t *testing.T) {
	// Any owner lookup must take this lock; path-only queries must not wait.
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
}

func BenchmarkSIDAccountCacheSameOwner(b *testing.B) {
	c := newSIDAccountCache(256)
	lookup := func() (string, bool) { return `DOMAIN\owner`, true }
	c.lookup("S-1-5-21-123", lookup)
	b.ReportAllocs()
	for b.Loop() {
		c.lookup("S-1-5-21-123", lookup)
	}
}

func TestProcessMetadataCacheKeepsInstanceIdentity(t *testing.T) {
	c := newMetaCache()
	k := procKey{pid: uint32(os.Getpid())}
	c.m[k] = procMeta{path: "cached", user: "owner"}
	if p, u := c.lookup(k); p != "cached" || u != "owner" {
		t.Fatal(fmt.Sprint(p, u))
	}
	c.prune(map[procKey]struct{}{})
	if len(c.m) != 0 {
		t.Fatal("dead process metadata retained")
	}
}
