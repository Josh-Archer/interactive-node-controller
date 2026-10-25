package controller

import (
	"fmt"
	"strings"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	"github.com/robfig/cron/v3"
)

type scheduledWindow struct {
	state string
	start time.Time
	end   time.Time
}

type parsedWindow struct {
	state    string
	schedule cron.Schedule
	duration time.Duration
	loc      *time.Location
}

// validateSchedule reports every leadTime and window field that cannot be
// evaluated. An empty schedule is valid. Callers must not apply windows when
// this returns an error.
func validateSchedule(spec availabilityv1alpha1.NodeActivitySpec) error {
	_, _, err := parseSchedule(spec)
	return err
}

// parseSchedule validates spec.leadTime and every window, then returns the
// compiled schedule only when every field is valid.
func parseSchedule(spec availabilityv1alpha1.NodeActivitySpec) (time.Duration, []parsedWindow, error) {
	var problems []string
	var lead time.Duration
	if spec.LeadTime != "" {
		parsed, err := time.ParseDuration(spec.LeadTime)
		if err != nil || parsed < 0 {
			problems = append(problems, fmt.Sprintf("spec.leadTime %q must be a non-negative duration", spec.LeadTime))
		} else {
			lead = parsed
		}
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	windows := make([]parsedWindow, 0, len(spec.Windows))
	for i, w := range spec.Windows {
		valid := true
		schedule, err := parser.Parse(w.Schedule)
		if err != nil {
			valid = false
			problems = append(problems, fmt.Sprintf("spec.windows[%d].schedule %q is not a valid five-field cron expression: %v", i, w.Schedule, err))
		}
		duration, err := time.ParseDuration(w.Duration)
		if err != nil || duration <= 0 {
			valid = false
			problems = append(problems, fmt.Sprintf("spec.windows[%d].duration %q must be a positive duration", i, w.Duration))
		}
		switch w.State {
		case "game", "interactive", "idle-allowed":
		default:
			valid = false
			problems = append(problems, fmt.Sprintf("spec.windows[%d].state %q must be game, interactive, or idle-allowed", i, w.State))
		}
		loc := time.UTC
		if w.TimeZone != "" {
			loaded, locErr := time.LoadLocation(w.TimeZone)
			if locErr != nil {
				valid = false
				problems = append(problems, fmt.Sprintf("spec.windows[%d].timeZone %q is not a valid IANA time zone: %v", i, w.TimeZone, locErr))
			} else {
				loc = loaded
			}
		}
		if valid {
			windows = append(windows, parsedWindow{state: w.State, schedule: schedule, duration: duration, loc: loc})
		}
	}
	if len(problems) > 0 {
		return 0, nil, fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return lead, windows, nil
}

// evaluateWindows returns the strongest active window, or the next window
// whose lead time has begun. Cron expressions use the standard five fields.
// An invalid leadTime, cron expression, duration, state, or time zone returns
// an error and no window.
func evaluateWindows(spec availabilityv1alpha1.NodeActivitySpec, now time.Time) (*scheduledWindow, time.Time, time.Duration, error) {
	lead, windows, err := parseSchedule(spec)
	if err != nil {
		return nil, time.Time{}, 0, err
	}
	var active *scheduledWindow
	var next time.Time
	var wake time.Time
	for _, w := range windows {
		localNow := now.In(w.loc)
		// Scan starts in the duration immediately preceding now. A cron tick at
		// the exact lower bound is included by advancing one nanosecond earlier.
		cursor := localNow.Add(-w.duration).Add(-time.Nanosecond)
		for start := w.schedule.Next(cursor); !start.After(localNow); start = w.schedule.Next(start) {
			end := start.Add(w.duration)
			if localNow.Before(end) {
				if wake.IsZero() || end.Before(wake) {
					wake = end
				}
				candidate := &scheduledWindow{state: w.state, start: start, end: end}
				if active == nil || windowRank(candidate.state) > windowRank(active.state) || (windowRank(candidate.state) == windowRank(active.state) && candidate.end.Before(active.end)) {
					active = candidate
				}
			}
		}
		upcoming := w.schedule.Next(localNow)
		if next.IsZero() || upcoming.Before(next) {
			next = upcoming
		}
		if lead > 0 && !localNow.Before(upcoming.Add(-lead)) && localNow.Before(upcoming) {
			if wake.IsZero() || upcoming.Before(wake) {
				wake = upcoming
			}
			candidate := &scheduledWindow{state: w.state, start: upcoming, end: upcoming.Add(w.duration)}
			if active == nil || windowRank(candidate.state) > windowRank(active.state) {
				active = candidate
			}
		}
	}
	var wait time.Duration
	if !wake.IsZero() {
		wait = wake.Sub(now)
	}
	if !next.IsZero() && (wait <= 0 || next.Sub(now) < wait) {
		wait = next.Sub(now)
	}
	if wait > 0 {
	} else if !next.IsZero() {
		wait = time.Second
	}
	return active, next, wait, nil
}

func windowRank(state string) int {
	switch strings.TrimSpace(state) {
	case "game":
		return 3
	case "interactive":
		return 2
	default:
		return 1
	}
}
