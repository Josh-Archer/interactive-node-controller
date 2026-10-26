package controller

import (
	"context"
	"testing"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	clocktesting "k8s.io/utils/clock/testing"
)

// Regressions for #75 and #76: when the desired taint is nil (the node admits
// new Pods) no lossy eviction may happen, even if the report or a leftover
// node taint still says interactive.
func TestQANilDesiredTaintNeverEvictsLossy(t *testing.T) {
	tests := []struct {
		name          string
		activity      availabilityv1alpha1.Activity
		stale         bool
		failClosedOff bool
	}{
		// #75: scheduled idle-allowed window with a fresh Active/Interactive report.
		{name: "idle-allowed window with fresh interactive report", activity: availabilityv1alpha1.ActivityInteractive},
		{name: "idle-allowed window with fresh active non-game report", activity: availabilityv1alpha1.ActivityUnknown},
		// #76: FailClosed=false with a stale heartbeat after an interactive report.
		{name: "stale heartbeat with fail-closed disabled", activity: availabilityv1alpha1.ActivityInteractive, stale: true, failClosedOff: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			standard, lossy := contractPods()
			r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, test.activity, corev1.TaintEffectNoSchedule, []*corev1.Pod{standard, lossy})
			if test.failClosedOff {
				r.Policy.FailClosed = false
			}
			if test.stale {
				r.Clock = clocktesting.NewFakeClock(activity.Status.HeartbeatAt.Add(2 * r.Policy.StaleAfter))
			}
			// The interactive PreferNoSchedule taint from the previous report is still on the node.
			node.Spec.Taints = []corev1.Taint{{Key: r.Policy.Key, Value: r.Policy.InteractiveValue, Effect: corev1.TaintEffectPreferNoSchedule}}
			if _, err := r.reconcileEvictions(context.Background(), activity, node, nil); err != nil {
				t.Fatal(err)
			}
			assertEvicted(t, evictor)
		})
	}
}
