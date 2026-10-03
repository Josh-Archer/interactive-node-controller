package controller

import (
	"context"
	"testing"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
)

func TestDuplicateEnrollmentsPreserveStrongestProtection(t *testing.T) {
	for _, namespace := range []string{"availability", "other"} {
		for _, idleFirst := range []bool{true, false} {
			t.Run(namespace+map[bool]string{true: "-idle-first", false: "-game-first"}[idleFirst], func(t *testing.T) {
				resetMetricsForTesting()
				defer resetMetricsForTesting()
				ctx := context.Background()
				r, active, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{eligiblePod("candidate")})
				r.Clock = clocktesting.NewFakeClock(active.Status.HeartbeatAt)
				idle := &availabilityv1alpha1.NodeActivity{ObjectMeta: metav1.ObjectMeta{Name: "duplicate", Namespace: namespace}, Spec: active.Spec}
				if err := r.Create(ctx, idle); err != nil {
					t.Fatal(err)
				}
				idle.Status = availabilityv1alpha1.NodeActivityStatus{State: availabilityv1alpha1.StateIdle, Activity: availabilityv1alpha1.ActivityIdle, HeartbeatAt: active.Status.HeartbeatAt}
				if err := r.Status().Update(ctx, idle); err != nil {
					t.Fatal(err)
				}
				reconcile := func(item *availabilityv1alpha1.NodeActivity) {
					t.Helper()
					if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: item.Name, Namespace: item.Namespace}}); err != nil {
						t.Fatal(err)
					}
					if err := r.Get(ctx, types.NamespacedName{Name: node.Name}, node); err != nil {
						t.Fatal(err)
					}
					if !hasTaint(node, r.Policy.Key, r.Policy.ActiveValue, corev1.TaintEffectNoSchedule) {
						t.Fatalf("active protection lost: %#v", node.Spec.Taints)
					}
				}
				order := []*availabilityv1alpha1.NodeActivity{active, idle}
				if idleFirst {
					order = []*availabilityv1alpha1.NodeActivity{idle, active}
				}
				for _, item := range order {
					reconcile(item)
				}
				if len(evictor.names) != 0 {
					t.Fatal("ambiguous enrollment allowed eviction")
				}
				if err := r.Get(ctx, types.NamespacedName{Name: idle.Name, Namespace: idle.Namespace}, idle); err != nil {
					t.Fatal(err)
				}
				if condition := meta.FindStatusCondition(idle.Status.Conditions, TaintAppliedCondition); condition == nil || condition.Reason != "DuplicateEnrollment" {
					t.Fatalf("condition=%#v", condition)
				}
				if err := r.Delete(ctx, idle); err != nil {
					t.Fatal(err)
				}
				reconcile(idle)
				if testutil.CollectAndCount(activityMetric) != 1 || testutil.CollectAndCount(taintMetric) != 1 {
					t.Fatal("deleting duplicate removed survivor metrics")
				}
				// A new reconciler must recover from resources, without process-local ownership.
				r = &NodeActivityReconciler{Client: r.Client, Policy: r.Policy, Clock: r.Clock}
				reconcile(active)
			})
		}
	}
}

func TestDuplicateEnrollmentConservativeStates(t *testing.T) {
	for _, test := range []struct {
		name     string
		state    availabilityv1alpha1.State
		activity availabilityv1alpha1.Activity
		stale    bool
		value    string
		effect   corev1.TaintEffect
	}{
		{name: "interactive", state: availabilityv1alpha1.StateActive, activity: availabilityv1alpha1.ActivityInteractive, value: "interactive", effect: corev1.TaintEffectPreferNoSchedule},
		{name: "unknown", state: availabilityv1alpha1.StateUnknown, activity: availabilityv1alpha1.ActivityUnknown, value: "unavailable", effect: corev1.TaintEffectNoSchedule},
		{name: "stale-game", state: availabilityv1alpha1.StateActive, activity: availabilityv1alpha1.ActivityGame, stale: true, value: "unavailable", effect: corev1.TaintEffectNoSchedule},
		{name: "idle", state: availabilityv1alpha1.StateIdle, activity: availabilityv1alpha1.ActivityIdle},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			r, idle, node, _ := evictionFixture(t, availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, corev1.TaintEffectNoSchedule, nil)
			r.Clock = clocktesting.NewFakeClock(idle.Status.HeartbeatAt)
			other := &availabilityv1alpha1.NodeActivity{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "other"}, Spec: idle.Spec}
			if err := r.Create(ctx, other); err != nil {
				t.Fatal(err)
			}
			other.Status = availabilityv1alpha1.NodeActivityStatus{State: test.state, Activity: test.activity, HeartbeatAt: idle.Status.HeartbeatAt}
			if test.stale {
				other.Status.HeartbeatAt = other.Status.HeartbeatAt.Add(-2 * time.Minute)
			}
			if err := r.Status().Update(ctx, other); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: idle.Namespace, Name: idle.Name}}); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, types.NamespacedName{Name: node.Name}, node); err != nil {
				t.Fatal(err)
			}
			if test.value == "" {
				if len(node.Spec.Taints) != 0 {
					t.Fatalf("taints=%#v", node.Spec.Taints)
				}
			} else if !hasTaint(node, r.Policy.Key, test.value, test.effect) {
				t.Fatalf("taints=%#v", node.Spec.Taints)
			}
			requests := r.requestsForNode(ctx, other)
			if len(requests) != 2 {
				t.Fatalf("sibling requests=%#v", requests)
			}
		})
	}
}
