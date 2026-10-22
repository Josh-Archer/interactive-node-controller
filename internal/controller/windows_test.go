package controller

import (
	"testing"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
)

func TestEvaluateWindowsBoundariesAndLeadTime(t *testing.T) {
	loc := time.UTC
	window := availabilityv1alpha1.AvailabilityWindow{Schedule: "0 20 * * *", Duration: "2h", TimeZone: "UTC", State: "game"}
	spec := availabilityv1alpha1.NodeActivitySpec{Windows: []availabilityv1alpha1.AvailabilityWindow{window}, LeadTime: "30m"}
	tests := []struct {
		name     string
		now      time.Time
		state    string
		active   bool
		wantWait time.Duration
	}{
		{"before lead time", time.Date(2026, 8, 24, 19, 0, 0, 0, loc), "", false, time.Hour},
		{"lead time begins", time.Date(2026, 8, 24, 19, 30, 0, 0, loc), "game", true, 30 * time.Minute},
		{"window start", time.Date(2026, 8, 24, 20, 0, 0, 0, loc), "game", true, 2 * time.Hour},
		{"window end is exclusive", time.Date(2026, 8, 24, 22, 0, 0, 0, loc), "", false, 22 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, next, wait, err := evaluateWindows(spec, tt.now)
			if err != nil {
				t.Fatal(err)
			}
			if (got != nil) != tt.active {
				t.Fatalf("active window = %#v", got)
			}
			if got != nil && got.state != tt.state {
				t.Fatalf("state = %q, want %q", got.state, tt.state)
			}
			if next.IsZero() || wait != tt.wantWait {
				t.Fatalf("next=%s wait=%s, want wait=%s", next, wait, tt.wantWait)
			}
		})
	}
}

func TestEvaluateWindowsUsesIANAZoneAcrossDST(t *testing.T) {
	spec := availabilityv1alpha1.NodeActivitySpec{Windows: []availabilityv1alpha1.AvailabilityWindow{{Schedule: "30 1 * * *", Duration: "90m", TimeZone: "America/New_York", State: "interactive"}}}
	// On the fall-back day, the 01:30 start is the first 01:30 occurrence.
	now := time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC)
	window, _, _, err := evaluateWindows(spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if window == nil || window.start.In(time.UTC).Hour() != 5 || window.start.In(time.UTC).Minute() != 30 {
		t.Fatalf("window start = %#v, want 2026-11-01 05:30 UTC", window)
	}
}

func TestDesiredTaintWindowOverridesInteractiveButNotLiveGame(t *testing.T) {
	now := time.Date(2026, 8, 24, 20, 30, 0, 0, time.UTC)
	activity := &availabilityv1alpha1.NodeActivity{Spec: availabilityv1alpha1.NodeActivitySpec{Windows: []availabilityv1alpha1.AvailabilityWindow{{Schedule: "0 20 * * *", Duration: "2h", TimeZone: "UTC", State: "game"}}}, Status: availabilityv1alpha1.NodeActivityStatus{State: availabilityv1alpha1.StateActive, Activity: availabilityv1alpha1.ActivityInteractive, HeartbeatAt: now}}
	r := &NodeActivityReconciler{Policy: testPolicy()}
	taint, _, _, scheduled, _, err := r.desiredTaint(activity, now)
	if err != nil || !scheduled || taint == nil || taint.Value != testPolicy().ActiveValue {
		t.Fatalf("window desired taint=%#v scheduled=%v err=%v", taint, scheduled, err)
	}
	activity.Status.Activity = availabilityv1alpha1.ActivityGame
	taint, _, _, scheduled, _, err = r.desiredTaint(activity, now)
	if err != nil || scheduled || taint == nil || taint.Value != testPolicy().ActiveValue {
		t.Fatalf("live game desired taint=%#v scheduled=%v err=%v", taint, scheduled, err)
	}
}
