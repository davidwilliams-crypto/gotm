//go:build windows

package ui

import (
	"testing"
	"time"
)

func TestStartedLabel(t *testing.T) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 14, 23, 5, 0, now.Location())
	if got, want := startedLabel(today), "14:23:05"; got != want {
		t.Fatalf("today=%q, want %q", got, want)
	}
	other := time.Date(now.Year(), now.Month(), now.Day(), 9, 5, 0, 0, now.Location()).AddDate(0, 0, -2)
	if other.Year() != now.Year() { // a 1 January run: use a date later this year
		other = other.AddDate(0, 0, 4)
	}
	if got, want := startedLabel(other), other.Format("Jan _2 15:04"); got != want {
		t.Fatalf("this year=%q, want %q", got, want)
	}
	old := time.Date(now.Year()-1, 3, 4, 9, 5, 0, 0, now.Location())
	if got, want := startedLabel(old), old.Format("2006-01-02 15:04"); got != want {
		t.Fatalf("previous year=%q, want %q", got, want)
	}
}
