//go:build windows

package metrics

import (
	"testing"
	"time"
)

func TestSubscribeCancelClosesChannel(t *testing.T) {
	s := NewSampler(1000, 60)
	ch, cancel := s.Subscribe()
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("the channel delivered a snapshot after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not close the channel")
	}
	cancel() // a second cancel must not panic or double-close
	if s.Subscribers() != 0 {
		t.Fatalf("subscribers = %d after cancel", s.Subscribers())
	}
}
