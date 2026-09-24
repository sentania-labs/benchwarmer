package humantime

import (
	"testing"
	"time"
)

func TestClock(t *testing.T) {
	chi, _ := time.LoadLocation("America/Chicago")
	now := time.Date(2026, 9, 23, 19, 0, 0, 0, chi)
	if got := Clock(now.Add(45*time.Minute), now, "America/Chicago"); got != "7:45 PM" {
		t.Fatal(got)
	}
	if got := Clock(now.Add(6*time.Hour), now, "America/Chicago"); got != "Thu Sep 24, 1:00 AM" {
		t.Fatal(got)
	}
	// A UTC instant is still rendered in the configured zone.
	if got := Clock(time.Date(2026, 9, 24, 0, 30, 0, 0, time.UTC), now, "America/Chicago"); got != "7:30 PM" {
		t.Fatal(got)
	}
}
