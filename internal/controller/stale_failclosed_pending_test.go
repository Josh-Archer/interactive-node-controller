package controller

import (
	"context"
	"testing"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	clocktesting "k8s.io/utils/clock/testing"
)

// A stale heartbeat evicts only once the fail-closed NoSchedule is applied
// (docs/controller.md stale row). While it is desired but not yet applied, a
// leftover interactive PreferNoSchedule taint and an Active/Interactive last
// report must not fall through to the interactive lossy-eviction row.
func TestStaleHeartbeatFailClosedNotYetAppliedDoesNotEvict(t *testing.T) {
	standard, lossy := contractPods()
	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityInteractive, corev1.TaintEffectNoSchedule, []*corev1.Pod{standard, lossy})
	r.Policy.FailClosed = true
	r.Clock = clocktesting.NewFakeClock(activity.Status.HeartbeatAt.Add(2 * r.Policy.StaleAfter))
	node.Spec.Taints = []corev1.Taint{{Key: r.Policy.Key, Value: r.Policy.InteractiveValue, Effect: corev1.TaintEffectPreferNoSchedule}}
	desired := &corev1.Taint{Key: r.Policy.Key, Value: r.Policy.FailClosedValue, Effect: corev1.TaintEffectNoSchedule}

	if _, err := r.reconcileEvictions(context.Background(), activity, node, desired); err != nil {
		t.Fatal(err)
	}
	assertEvicted(t, evictor)
}
