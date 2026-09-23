// Package schedule resolves which policy profile is active at an instant.
// Schedules are wall-clock windows in the configured IANA zone, so they
// follow daylight-saving changes: 07:00 means 07:00 local on both sides of a
// transition.
package schedule

import (
	"time"

	"github.com/sentania-labs/benchwarmer/internal/config"
)

// Active reports the profile in force at now and which schedule selected it.
// source is "schedule:<name>" or "default". The first enabled matching
// schedule wins. A window with End before Start spans midnight and belongs to
// the day it starts on.
func Active(c config.Config, now time.Time) (profile, source string) {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		// Validation rejects bad zones; fail safe to the default profile.
		return c.DefaultProfile, "default"
	}
	local := now.In(loc)
	for _, s := range c.Schedules {
		if s.Enabled && inWindow(s, local) {
			return s.Profile, "schedule:" + s.Name
		}
	}
	return c.DefaultProfile, "default"
}

func inWindow(s config.Schedule, local time.Time) bool {
	start, err1 := config.ClockMinutes(s.Start)
	end, err2 := config.ClockMinutes(s.End)
	if err1 != nil || err2 != nil {
		return false
	}
	m := local.Hour()*60 + local.Minute()
	today := local.Weekday()
	if start < end {
		return hasDay(s, today) && m >= start && m < end
	}
	// Overnight: the evening part belongs to today, the morning part to
	// yesterday's window.
	if m >= start && hasDay(s, today) {
		return true
	}
	return m < end && hasDay(s, (today+6)%7)
}

func hasDay(s config.Schedule, d time.Weekday) bool {
	for _, x := range s.Days {
		if w, ok := config.Weekday(x); ok && w == d {
			return true
		}
	}
	return false
}

// NextChange returns the next instant after now at which the active profile
// changes, searching up to eight days ahead at minute resolution. Zero means
// no change in that horizon.
func NextChange(c config.Config, now time.Time) time.Time {
	cur, curSrc := Active(c, now)
	t := now.Truncate(time.Minute).Add(time.Minute)
	for end := now.Add(8 * 24 * time.Hour); t.Before(end); t = t.Add(time.Minute) {
		if p, src := Active(c, t); p != cur || src != curSrc {
			return t
		}
	}
	return time.Time{}
}
