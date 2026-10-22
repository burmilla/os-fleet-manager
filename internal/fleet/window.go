package fleet

import (
	"fmt"
	"strings"
	"time"
)

// MaintenanceWindow is a weekly recurring time window in which a server
// is allowed to upgrade (and reboot).
type MaintenanceWindow struct {
	// Days of week (mon, tue, ...). Empty means every day.
	Days []string `yaml:"days,omitempty" json:"days"`
	// Start time of day, HH:MM.
	Start string `yaml:"start" json:"start"`
	// Duration as Go duration, e.g. 2h or 90m.
	Duration string `yaml:"duration" json:"duration"`
	// IANA time zone name, defaults to UTC.
	Timezone string `yaml:"timezone,omitempty" json:"timezone"`
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

func parseDay(s string) (time.Weekday, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) >= 3 {
		s = s[:3]
	}
	d, ok := weekdays[s]
	return d, ok
}

func (w *MaintenanceWindow) Validate() error {
	for _, day := range w.Days {
		if _, ok := parseDay(day); !ok {
			return fmt.Errorf("invalid day %q", day)
		}
	}
	if _, err := time.Parse("15:04", w.Start); err != nil {
		return fmt.Errorf("invalid start %q, expected HH:MM", w.Start)
	}
	d, err := time.ParseDuration(w.Duration)
	if err != nil || d <= 0 || d > 7*24*time.Hour {
		return fmt.Errorf("invalid duration %q", w.Duration)
	}
	if _, err := time.LoadLocation(w.Timezone); err != nil {
		return fmt.Errorf("invalid timezone %q", w.Timezone)
	}
	return nil
}

func (w *MaintenanceWindow) parts() (loc *time.Location, hour, min int, dur time.Duration, days map[time.Weekday]bool) {
	loc, err := time.LoadLocation(w.Timezone)
	if err != nil {
		loc = time.UTC
	}
	start, _ := time.Parse("15:04", w.Start)
	dur, _ = time.ParseDuration(w.Duration)
	if len(w.Days) > 0 {
		days = map[time.Weekday]bool{}
		for _, s := range w.Days {
			if d, ok := parseDay(s); ok {
				days[d] = true
			}
		}
	}
	return loc, start.Hour(), start.Minute(), dur, days
}

// ActiveUntil reports whether now is inside the window and when it ends.
func (w *MaintenanceWindow) ActiveUntil(now time.Time) (time.Time, bool) {
	loc, hour, min, dur, days := w.parts()
	t := now.In(loc)
	// A window can start on an earlier day and still be open now.
	back := int(dur/(24*time.Hour)) + 1
	for i := 0; i <= back; i++ {
		day := t.AddDate(0, 0, -i)
		start := time.Date(day.Year(), day.Month(), day.Day(), hour, min, 0, 0, loc)
		if days != nil && !days[start.Weekday()] {
			continue
		}
		end := start.Add(dur)
		if !t.Before(start) && t.Before(end) {
			return end, true
		}
	}
	return time.Time{}, false
}

// NextStart returns the next start of the window after now.
func (w *MaintenanceWindow) NextStart(now time.Time) (time.Time, bool) {
	loc, hour, min, _, days := w.parts()
	t := now.In(loc)
	for i := 0; i <= 8; i++ {
		day := t.AddDate(0, 0, i)
		start := time.Date(day.Year(), day.Month(), day.Day(), hour, min, 0, 0, loc)
		if days != nil && !days[start.Weekday()] {
			continue
		}
		if start.After(t) {
			return start, true
		}
	}
	return time.Time{}, false
}
