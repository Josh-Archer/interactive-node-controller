package engine

import (
	"testing"
	"time"

	v1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	"github.com/Josh-Archer/interactive-node-controller/internal/reporter"
	"github.com/Josh-Archer/interactive-node-controller/internal/signals"
)

func TestEvaluatorDebouncePrecedenceAndGeneration(t *testing.T) {
	evaluator := New(time.Minute, DebounceSamples{Game: 1, Interactive: 2, Idle: 2})
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

	status := evaluator.Next(observe(signals.ActivityGame), now)
	assertStatus(t, status, reporter.StateActive, signals.ActivityGame, 1)

	status = evaluator.Next(observe(signals.ActivityInteractive), now.Add(time.Second))
	assertStatus(t, status, reporter.StateActive, signals.ActivityGame, 1)

	status = evaluator.Next(observe(signals.ActivityInteractive), now.Add(2*time.Second))
	assertStatus(t, status, reporter.StateActive, signals.ActivityInteractive, 2)

	status = evaluator.Next(observe(signals.ActivityIdle), now.Add(3*time.Second))
	assertStatus(t, status, reporter.StateActive, signals.ActivityInteractive, 2)
	status = evaluator.Next(observe(signals.ActivityIdle), now.Add(4*time.Second))
	assertStatus(t, status, reporter.StateIdle, signals.ActivityIdle, 3)
}

func TestEvaluatorUnknownThenStaleFailClosed(t *testing.T) {
	evaluator := New(10*time.Second, DebounceSamples{Game: 1, Interactive: 1, Idle: 1})
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	status := evaluator.Next(observe(signals.ActivityIdle), now)
	assertStatus(t, status, reporter.StateIdle, signals.ActivityIdle, 1)

	unknown := []signals.Observation{{Provider: "logind", Activity: signals.ActivityUnknown, Reason: "unavailable"}}
	status = evaluator.Next(unknown, now.Add(5*time.Second))
	assertStatus(t, status, reporter.StateUnknown, signals.ActivityUnknown, 2)
	if !status.FailClosed || !status.ObservedAt.Equal(now) {
		t.Fatalf("unknown status lost fail-closed/last-observation semantics: %#v", status)
	}
	status = evaluator.Next(unknown, now.Add(11*time.Second))
	assertStatus(t, status, reporter.StateStale, signals.ActivityUnknown, 3)
	if !status.HeartbeatAt.Equal(now.Add(11 * time.Second)) {
		t.Fatalf("heartbeat = %s", status.HeartbeatAt)
	}
}

func TestInitialDebounceIsExplicitUnknown(t *testing.T) {
	evaluator := New(time.Minute, DebounceSamples{Game: 1, Interactive: 2, Idle: 3})
	status := evaluator.Next(observe(signals.ActivityInteractive), time.Now())
	assertStatus(t, status, reporter.StateUnknown, signals.ActivityUnknown, 1)
}

func observe(activity signals.Activity) []signals.Observation {
	return []signals.Observation{{Provider: "test", Activity: activity, Reason: "test observation"}}
}

func assertStatus(t *testing.T, got reporter.Status, state reporter.State, activity signals.Activity, generation int64) {
	t.Helper()
	if got.State != state || got.Activity != v1alpha1.Activity(activity) || got.Generation != generation {
		t.Fatalf("status = state %q activity %q generation %d; want %q %q %d", got.State, got.Activity, got.Generation, state, activity, generation)
	}
}
