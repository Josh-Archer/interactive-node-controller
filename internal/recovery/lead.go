package recovery

import (
	"fmt"
	"time"
)

// LeadWindow is a single UTC cron occurrence whose lead boundary is a few
// seconds ahead of now. The Kind harness uses it to observe scheduled tainting.
// The lead is shorter than EndToEnd, so the harness must not claim resources
// were released before the window.
type LeadWindow struct {
	Schedule string
	Lead     time.Duration
	Boundary time.Time
	Start    time.Time
	Duration string
	State    string
}

// NextLeadWindow picks the soonest minute boundary that is at least 25s ahead
// and places the lead boundary about 10s after now.
func NextLeadWindow(now time.Time) (LeadWindow, error) {
	now = now.UTC()
	start := now.Truncate(time.Minute).Add(time.Minute)
	for start.Sub(now) < 25*time.Second {
		start = start.Add(time.Minute)
	}
	lead := (start.Sub(now) - 10*time.Second).Truncate(time.Second)
	if lead < time.Second {
		return LeadWindow{}, fmt.Errorf("lead for %s is not positive", start.Format(time.RFC3339))
	}
	boundary := start.Add(-lead)
	if !boundary.After(now) || !start.After(boundary) {
		return LeadWindow{}, fmt.Errorf("lead boundary %s is not between %s and %s", boundary, now, start)
	}
	return LeadWindow{
		Schedule: fmt.Sprintf("%d %d * * *", start.Minute(), start.Hour()),
		Lead:     lead,
		Boundary: boundary,
		Start:    start,
		Duration: "3m",
		State:    "game",
	}, nil
}
