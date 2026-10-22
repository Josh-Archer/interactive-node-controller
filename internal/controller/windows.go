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

// evaluateWindows returns the strongest active window, or the next window
// whose lead time has begun. Cron expressions use the standard five fields.
func evaluateWindows(spec availabilityv1alpha1.NodeActivitySpec, now time.Time) (*scheduledWindow, time.Time, time.Duration, error) {
	var active *scheduledWindow
	var next time.Time
	var wake time.Time
	lead := time.Duration(0)
	if spec.LeadTime != "" {
		var err error
		lead, err = time.ParseDuration(spec.LeadTime)
		if err != nil || lead < 0 {
			return nil, time.Time{}, 0, fmt.Errorf("spec.leadTime must be a non-negative duration")
		}
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	for _, w := range spec.Windows {
		schedule, err := parser.Parse(w.Schedule)
		if err != nil {
			return nil, time.Time{}, 0, fmt.Errorf("invalid window schedule %q: %w", w.Schedule, err)
		}
		duration, err := time.ParseDuration(w.Duration)
		if err != nil || duration <= 0 {
			return nil, time.Time{}, 0, fmt.Errorf("window duration must be positive")
		}
		if w.State != "game" && w.State != "interactive" && w.State != "idle-allowed" {
			return nil, time.Time{}, 0, fmt.Errorf("window state must be game, interactive, or idle-allowed")
		}
		loc := time.UTC
		if w.TimeZone != "" {
			loc, err = time.LoadLocation(w.TimeZone)
			if err != nil {
				return nil, time.Time{}, 0, fmt.Errorf("invalid window timeZone %q: %w", w.TimeZone, err)
			}
		}
		localNow := now.In(loc)
		// Scan starts in the duration immediately preceding now. A cron tick at
		// the exact lower bound is included by advancing one nanosecond earlier.
		cursor := localNow.Add(-duration).Add(-time.Nanosecond)
		for start := schedule.Next(cursor); !start.After(localNow); start = schedule.Next(start) {
			end := start.Add(duration)
			if localNow.Before(end) {
				if wake.IsZero() || end.Before(wake) {
					wake = end
				}
				candidate := &scheduledWindow{state: w.State, start: start, end: end}
				if active == nil || windowRank(candidate.state) > windowRank(active.state) || (windowRank(candidate.state) == windowRank(active.state) && candidate.end.Before(active.end)) {
					active = candidate
				}
			}
		}
		upcoming := schedule.Next(localNow)
		if next.IsZero() || upcoming.Before(next) {
			next = upcoming
		}
		if lead > 0 && !localNow.Before(upcoming.Add(-lead)) && localNow.Before(upcoming) {
			if wake.IsZero() || upcoming.Before(wake) {
				wake = upcoming
			}
			candidate := &scheduledWindow{state: w.State, start: upcoming, end: upcoming.Add(duration)}
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
