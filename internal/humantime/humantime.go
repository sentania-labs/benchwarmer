// Package humantime renders instants for people: a local clock time in the
// configured zone, with the date when it is not the same local day as the
// reference instant. Machine-readable fields keep RFC 3339; anything a person
// reads (reasons, summaries, messages) uses this.
package humantime

import "time"

// Clock formats t in zone tz relative to now, e.g. "7:45 PM" or
// "Thu Sep 24, 7:45 PM". An unknown zone falls back to the host zone.
func Clock(t, now time.Time, tz string) string {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.Local
	}
	lt, ln := t.In(loc), now.In(loc)
	if lt.Year() == ln.Year() && lt.YearDay() == ln.YearDay() {
		return lt.Format("3:04 PM")
	}
	return lt.Format("Mon Jan 2, 3:04 PM")
}
