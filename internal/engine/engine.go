package engine

import (
	"fmt"
	"time"

	v1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	"github.com/Josh-Archer/interactive-node-controller/internal/reporter"
	"github.com/Josh-Archer/interactive-node-controller/internal/signals"
)

type DebounceSamples struct {
	Game        int
	Interactive int
	Idle        int
}

type Evaluator struct {
	staleAfter time.Duration
	samples    DebounceSamples

	current      signals.Activity
	pending      signals.Activity
	pendingCount int
	lastGood     time.Time
	lastObserved time.Time
	lastState    reporter.State
	lastActivity signals.Activity
	generation   int64
}

func New(staleAfter time.Duration, samples DebounceSamples) *Evaluator {
	return &Evaluator{staleAfter: staleAfter, samples: samples, current: signals.ActivityUnknown}
}

func (e *Evaluator) Next(observations []signals.Observation, now time.Time) reporter.Status {
	candidate, reason := signals.Aggregate(observations)
	state := reporter.StateUnknown
	activity := signals.ActivityUnknown
	observedAt := now

	if candidate == signals.ActivityUnknown {
		e.pending = signals.ActivityUnknown
		e.pendingCount = 0
		if !e.lastGood.IsZero() {
			observedAt = e.lastGood
			if now.Sub(e.lastGood) >= e.staleAfter {
				state = reporter.StateStale
				reason = fmt.Sprintf("last successful observation is stale: %s", reason)
			}
		}
	} else {
		e.lastGood = now
		observedAt = now
		if candidate == e.current {
			e.pending = signals.ActivityUnknown
			e.pendingCount = 0
			activity = e.current
			state = stateFor(activity)
		} else {
			if candidate != e.pending {
				e.pending = candidate
				e.pendingCount = 1
			} else {
				e.pendingCount++
			}
			required := e.requiredSamples(candidate)
			if e.pendingCount >= required {
				e.current = candidate
				e.pending = signals.ActivityUnknown
				e.pendingCount = 0
				activity = candidate
				state = stateFor(activity)
			} else if e.current != signals.ActivityUnknown {
				activity = e.current
				state = stateFor(activity)
				reason = fmt.Sprintf("debouncing %s (%d/%d); retaining %s", candidate, e.pendingCount, required, e.current)
			} else {
				reason = fmt.Sprintf("debouncing initial %s (%d/%d): %s", candidate, e.pendingCount, required, reason)
			}
		}
	}

	if e.generation == 0 || state != e.lastState || activity != e.lastActivity {
		e.generation++
		e.lastState = state
		e.lastActivity = activity
		e.lastObserved = observedAt
	} else if state == reporter.StateActive || state == reporter.StateIdle {
		e.lastObserved = observedAt
	}
	if e.lastObserved.IsZero() {
		e.lastObserved = observedAt
	}

	return reporter.Status{
		State:       state,
		Activity:    v1alpha1.Activity(activity),
		Reason:      reason,
		ObservedAt:  e.lastObserved.UTC(),
		HeartbeatAt: now.UTC(),
		Generation:  e.generation,
		FailClosed:  true,
	}
}

func (e *Evaluator) requiredSamples(activity signals.Activity) int {
	switch activity {
	case signals.ActivityGame:
		return e.samples.Game
	case signals.ActivityInteractive:
		return e.samples.Interactive
	case signals.ActivityIdle:
		return e.samples.Idle
	default:
		return 1
	}
}

func stateFor(activity signals.Activity) reporter.State {
	switch activity {
	case signals.ActivityGame, signals.ActivityInteractive:
		return reporter.StateActive
	case signals.ActivityIdle:
		return reporter.StateIdle
	default:
		return reporter.StateUnknown
	}
}
