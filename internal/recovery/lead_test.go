package recovery

import (
	"testing"
	"time"
)

func TestNextLeadWindow(t *testing.T) {
	cases := []time.Time{
		time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 25, 12, 0, 40, 0, time.UTC),
		time.Date(2026, 10, 25, 23, 59, 50, 0, time.UTC),
	}
	for _, now := range cases {
		window, err := NextLeadWindow(now)
		if err != nil {
			t.Fatalf("%s: %v", now, err)
		}
		if !window.Boundary.After(now) || !window.Start.After(window.Boundary) {
			t.Fatalf("%s: boundary %s start %s", now, window.Boundary, window.Start)
		}
		if window.Start.Sub(window.Boundary) != window.Lead {
			t.Fatalf("%s: lead %s does not match start-boundary", now, window.Lead)
		}
		if gap := window.Boundary.Sub(now); gap < 9*time.Second || gap > 11*time.Second {
			t.Fatalf("%s: boundary gap = %s", now, gap)
		}
		if window.State != "game" || window.Duration != "3m" || window.Schedule == "" {
			t.Fatalf("%s: %+v", now, window)
		}
		if window.Lead >= DefaultBudget().EndToEnd.Duration() {
			t.Fatalf("%s: harness lead %s must stay shorter than the end-to-end budget", now, window.Lead)
		}
	}
}
