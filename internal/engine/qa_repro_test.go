package engine

import (
	"testing"
	"time"

	"github.com/Josh-Archer/interactive-node-controller/internal/reporter"
	"github.com/Josh-Archer/interactive-node-controller/internal/signals"
)

// Regression for #77: a reporter that never had a good observation must go from
// unknown to stale after stale_after, as docs/host-reporter.md promises.
func TestQANeverGoodObservationNeverStale(t *testing.T) {
	evaluator := New(time.Minute, DebounceSamples{Game: 1, Interactive: 1, Idle: 1})
	start := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	unknown := []signals.Observation{{Provider: "logind", Activity: signals.ActivityUnknown, Reason: "unavailable"}}

	status := evaluator.Next(unknown, start)
	if status.State != reporter.StateUnknown {
		t.Fatalf("first unknown observation: state = %s, want unknown", status.State)
	}
	status = evaluator.Next(unknown, start.Add(30*time.Second))
	if status.State != reporter.StateUnknown {
		t.Fatalf("before stale_after: state = %s, want unknown", status.State)
	}
	status = evaluator.Next(unknown, start.Add(10*time.Minute))
	if status.State != reporter.StateStale {
		t.Fatalf("10m with no good observation: state = %s, want stale", status.State)
	}
	if !status.FailClosed {
		t.Fatal("stale status must fail closed")
	}
	if !status.ObservedAt.Equal(start) {
		t.Fatalf("ObservedAt = %s, want reporter start %s", status.ObservedAt, start)
	}
}
