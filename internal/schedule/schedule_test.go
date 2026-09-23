package schedule

import (
	"testing"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/config"
)

var chicago, _ = time.LoadLocation("America/Chicago")

func at(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, chicago)
}

func TestSchoolHours(t *testing.T) {
	c := config.Default()
	cases := []struct {
		name string
		t    time.Time
		want string
	}{
		{"wed before", at(2026, 9, 23, 6, 59), "normal"},
		{"wed start", at(2026, 9, 23, 7, 0), "school_hours"},
		{"wed midday", at(2026, 9, 23, 12, 0), "school_hours"},
		{"wed last minute", at(2026, 9, 23, 16, 59), "school_hours"},
		{"wed end exclusive", at(2026, 9, 23, 17, 0), "normal"},
		{"saturday", at(2026, 9, 26, 12, 0), "normal"},
		{"sunday", at(2026, 9, 27, 12, 0), "normal"},
		{"friday", at(2026, 9, 25, 9, 0), "school_hours"},
	}
	for _, tc := range cases {
		if got, _ := Active(c, tc.t); got != tc.want {
			t.Errorf("%s (%s): got %s want %s", tc.name, tc.t, got, tc.want)
		}
	}
}

func TestEvaluatedInConfiguredZoneNotHostZone(t *testing.T) {
	c := config.Default()
	// 12:30 UTC on a Wednesday in September is 07:30 CDT: school hours.
	utc := time.Date(2026, 9, 23, 12, 30, 0, 0, time.UTC)
	if got, src := Active(c, utc); got != "school_hours" || src != "schedule:school-hours" {
		t.Fatalf("got %s %s", got, src)
	}
	// Same UTC clock in January is 06:30 CST: before school hours.
	if got, _ := Active(c, time.Date(2027, 1, 13, 12, 30, 0, 0, time.UTC)); got != "normal" {
		t.Fatalf("winter: got %s", got)
	}
}

func TestDaylightSavingTransitions(t *testing.T) {
	c := config.Default()
	// 2027-03-15 (Mon) is the day after spring-forward (Mar 14 2027); school
	// hours start at 07:00 CDT = 12:00 UTC.
	if got, _ := Active(c, time.Date(2027, 3, 15, 11, 59, 0, 0, time.UTC)); got != "normal" {
		t.Fatalf("06:59 CDT: got %s", got)
	}
	if got, _ := Active(c, time.Date(2027, 3, 15, 12, 0, 0, 0, time.UTC)); got != "school_hours" {
		t.Fatalf("07:00 CDT: got %s", got)
	}
	// Friday before (Mar 12, CST): 07:00 CST = 13:00 UTC.
	if got, _ := Active(c, time.Date(2027, 3, 12, 12, 30, 0, 0, time.UTC)); got != "normal" {
		t.Fatalf("06:30 CST: got %s", got)
	}
	// Fall back 2026-11-01 is a Sunday; Monday Nov 2 07:00 CST = 13:00 UTC.
	if got, _ := Active(c, time.Date(2026, 11, 2, 12, 30, 0, 0, time.UTC)); got != "normal" {
		t.Fatalf("06:30 CST after fall back: got %s", got)
	}
	if got, _ := Active(c, time.Date(2026, 11, 2, 13, 0, 0, 0, time.UTC)); got != "school_hours" {
		t.Fatalf("07:00 CST after fall back: got %s", got)
	}
}

func TestWindowAcrossDSTNight(t *testing.T) {
	c := config.Default()
	// A 01:00-03:00 window on the spring-forward night: 02:xx does not exist,
	// 01:30 and 03:00 do.
	c.Schedules = []config.Schedule{{Name: "night", Profile: "school_hours", Days: []string{"sun"}, Start: "01:00", End: "03:00", Enabled: true}}
	if got, _ := Active(c, at(2027, 3, 14, 1, 30)); got != "school_hours" {
		t.Fatal("01:30 should be inside")
	}
	if got, _ := Active(c, at(2027, 3, 14, 3, 0)); got != "normal" {
		t.Fatal("03:00 should be outside")
	}
}

func TestOvernightWindow(t *testing.T) {
	c := config.Default()
	c.Schedules = []config.Schedule{{Name: "late", Profile: "school_hours", Days: []string{"fri"}, Start: "22:00", End: "02:00", Enabled: true}}
	if got, _ := Active(c, at(2026, 9, 25, 23, 0)); got != "school_hours" {
		t.Fatal("fri 23:00")
	}
	if got, _ := Active(c, at(2026, 9, 26, 1, 0)); got != "school_hours" {
		t.Fatal("sat 01:00 belongs to friday's window")
	}
	if got, _ := Active(c, at(2026, 9, 27, 1, 0)); got != "normal" {
		t.Fatal("sun 01:00 is saturday's window, not scheduled")
	}
}

func TestDisabledScheduleIgnored(t *testing.T) {
	c := config.Default()
	c.Schedules[0].Enabled = false
	if got, _ := Active(c, at(2026, 9, 23, 12, 0)); got != "normal" {
		t.Fatal("disabled schedule applied")
	}
}

func TestNextChange(t *testing.T) {
	c := config.Default()
	got := NextChange(c, at(2026, 9, 23, 12, 0))
	if !got.Equal(at(2026, 9, 23, 17, 0)) {
		t.Fatalf("got %s", got.In(chicago))
	}
	// Friday evening: next change is Monday 07:00.
	got = NextChange(c, at(2026, 9, 25, 18, 0))
	if !got.Equal(at(2026, 9, 28, 7, 0)) {
		t.Fatalf("got %s", got.In(chicago))
	}
}
