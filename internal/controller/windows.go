package controller

import (
	"fmt"
	"strings"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	"github.com/robfig/cron/v3"
)

// maxWindowOccurrences bounds how many past cron activations one window may
// visit. A long duration combined with a frequent schedule is rejected so
// reconciliation cannot stall walking schedule.Next.
const maxWindowOccurrences = 4096

type scheduledWindow struct {
	state string
	start time.Time
	end   time.Time
}

type parsedWindow struct {
	expr     string
	label    string
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
			windows = append(windows, parsedWindow{expr: w.Schedule, label: w.Duration, state: w.State, schedule: schedule, duration: duration, loc: loc})
		}
	}
	if len(problems) > 0 {
		return 0, nil, fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return lead, windows, nil
}

// evaluateWindows returns the strongest open window, including one whose lead
// time has begun, the next window start, and the wait until the earliest
// lead-time, start, or end boundary. The schedule is validated by
// parseSchedule first: an invalid leadTime, cron expression, duration, state,
// or time zone returns an error and no window.
func evaluateWindows(spec availabilityv1alpha1.NodeActivitySpec, now time.Time) (*scheduledWindow, time.Time, time.Duration, error) {
	lead, windows, err := parseSchedule(spec)
	if err != nil {
		return nil, time.Time{}, 0, err
	}
	var active *scheduledWindow
	var next time.Time
	var wake time.Time
	for _, w := range windows {
		opened, upcoming, boundary, scanErr := scanWindow(w.expr, w.label, w.state, w.schedule, now.In(w.loc), w.duration, lead)
		if scanErr != nil {
			return nil, time.Time{}, 0, scanErr
		}
		active = preferOpenWindow(active, opened)
		if next.IsZero() || upcoming.Before(next) {
			next = upcoming
		}
		wake = earlierTime(wake, boundary)
	}
	if wake.IsZero() {
		return active, next, 0, nil
	}
	wait := wake.Sub(now)
	if wait <= 0 {
		return nil, time.Time{}, 0, fmt.Errorf("window schedule did not advance the requeue time")
	}
	return active, next, wait, nil
}

// scanWindow walks occurrences that can still be open and the next lead-time
// boundary. Ends are exclusive: a start exactly duration ago is not open.
func scanWindow(expr, durationLabel, state string, schedule cron.Schedule, localNow time.Time, duration, lead time.Duration) (*scheduledWindow, time.Time, time.Time, error) {
	cursor := localNow.Add(-duration)
	var active *scheduledWindow
	var wake time.Time
	steps := 0
	var upcoming time.Time
	for {
		start, err := nextActivation(expr, schedule, cursor)
		if err != nil {
			return nil, time.Time{}, time.Time{}, err
		}
		if start.After(localNow) {
			upcoming = start
			break
		}
		steps++
		if steps > maxWindowOccurrences {
			return nil, time.Time{}, time.Time{}, fmt.Errorf("window schedule %q with duration %s exceeds %d occurrences; shorten the duration or use a less frequent schedule", expr, durationLabel, maxWindowOccurrences)
		}
		end := start.Add(duration)
		if localNow.Before(end) {
			wake = earlierTime(wake, end)
			active = preferOpenWindow(active, &scheduledWindow{state: state, start: start, end: end})
		}
		cursor = start
	}
	// The upcoming start is always a boundary. When lead time is set, the
	// earliest future lead boundary can belong to a later occurrence than the
	// one Next(now) returns, so anchor that search at now+lead.
	wake = earlierTime(wake, upcoming)
	if lead > 0 {
		anchor, err := nextActivation(expr, schedule, localNow.Add(lead))
		if err != nil {
			return nil, time.Time{}, time.Time{}, err
		}
		leadBoundary := anchor.Add(-lead)
		if !leadBoundary.After(localNow) {
			return nil, time.Time{}, time.Time{}, fmt.Errorf("window schedule %q lead time did not advance past %s", expr, localNow.Format(time.RFC3339Nano))
		}
		wake = earlierTime(wake, leadBoundary)
		if !localNow.Before(upcoming.Add(-lead)) && localNow.Before(upcoming) {
			active = preferOpenWindow(active, &scheduledWindow{state: state, start: upcoming, end: upcoming.Add(duration)})
		}
	}
	return active, upcoming, wake, nil
}

func nextActivation(expr string, schedule cron.Schedule, after time.Time) (time.Time, error) {
	next := schedule.Next(after)
	if next.IsZero() {
		return time.Time{}, fmt.Errorf("window schedule %q has no occurrence within the supported horizon", expr)
	}
	if !next.After(after) {
		return time.Time{}, fmt.Errorf("window schedule %q did not advance past %s", expr, after.Format(time.RFC3339Nano))
	}
	return next, nil
}

func earlierTime(current, candidate time.Time) time.Time {
	if candidate.IsZero() {
		return current
	}
	if current.IsZero() || candidate.Before(current) {
		return candidate
	}
	return current
}

func preferOpenWindow(current, candidate *scheduledWindow) *scheduledWindow {
	if candidate == nil {
		return current
	}
	if current == nil || windowRank(candidate.state) > windowRank(current.state) || (windowRank(candidate.state) == windowRank(current.state) && candidate.end.Before(current.end)) {
		return candidate
	}
	return current
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
