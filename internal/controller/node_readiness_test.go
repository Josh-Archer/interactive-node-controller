package controller

import (
	"context"
	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"testing"
	"time"
)

// Existing policy fixtures explicitly model a Ready worker; outage tests below
// supply missing/False/Unknown conditions instead of silently defaulting Ready.
func readyNodeStatus() corev1.NodeStatus {
	return corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(time.Unix(1, 0))}}}
}
func TestNodeReadinessGate(t *testing.T) {
	now := time.Date(2026, 11, 15, 1, 26, 0, 0, time.UTC)
	r := &NodeActivityReconciler{Policy: testPolicy()}
	for _, tc := range []struct {
		name                  string
		status                corev1.ConditionStatus
		transition, heartbeat time.Time
		missing               bool
		reason                string
	}{
		{name: "ready", status: corev1.ConditionTrue, transition: now.Add(-time.Hour), heartbeat: now},
		{name: "false", status: corev1.ConditionFalse, transition: now, heartbeat: now, reason: "node-not-ready"},
		{name: "unknown", status: corev1.ConditionUnknown, transition: now, heartbeat: now, reason: "node-not-ready"},
		{name: "missing", missing: true, heartbeat: now, reason: "node-not-ready"},
		{name: "old report", status: corev1.ConditionTrue, transition: now, heartbeat: now.Add(-time.Second), reason: "node-recovery-pending"},
		{name: "zero transition", status: corev1.ConditionTrue, heartbeat: now, reason: "node-recovery-pending"},
		{name: "post recovery", status: corev1.ConditionTrue, transition: now, heartbeat: now},
		{name: "future transition", status: corev1.ConditionTrue, transition: now.Add(time.Hour), heartbeat: now, reason: "node-recovery-pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := &corev1.Node{}
			if !tc.missing {
				node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: tc.status, LastTransitionTime: metav1.NewTime(tc.transition)}}
			}
			p := enrollmentProtection{source: &availabilityv1alpha1.NodeActivity{Status: availabilityv1alpha1.NodeActivityStatus{State: availabilityv1alpha1.StateIdle, Activity: availabilityv1alpha1.ActivityIdle, HeartbeatAt: tc.heartbeat}}, admissionReason: "fresh-idle"}
			got := r.withNodeReadiness(p, node)
			if tc.reason == "" {
				if got.taint != nil {
					t.Fatalf("unexpected fence: %+v", got)
				}
			} else {
				if got.admissionReason != tc.reason || got.taint == nil || got.taint.Effect != corev1.TaintEffectNoSchedule || got.requeueAfter <= 0 {
					t.Fatalf("missing fence: %+v", got)
				}
			}
		})
	}
	if got := r.withNodeReadiness(enrollmentProtection{}, &corev1.Node{}); got.taint != nil {
		t.Fatal("final enrollment cleanup must not add fence")
	}
}
func TestNodeReadyWatchIgnoresHeartbeatButTracksRecovery(t *testing.T) {
	r := &NodeActivityReconciler{Policy: testPolicy()}
	p := r.nodeOwnedFieldsChanged()
	a := &corev1.Node{Status: readyNodeStatus()}
	b := a.DeepCopy()
	b.Status.Conditions[0].LastHeartbeatTime = metav1.Now()
	if p.Update(event.UpdateEvent{ObjectOld: a, ObjectNew: b}) {
		t.Fatal("routine heartbeat enqueued")
	}
	b.Status.Conditions[0].LastTransitionTime = metav1.Now()
	if !p.Update(event.UpdateEvent{ObjectOld: a, ObjectNew: b}) {
		t.Fatal("recovery transition ignored")
	}
}

func TestReconcileNodeOutageDoesNotBroadenEviction(t *testing.T) {
	for _, state := range []availabilityv1alpha1.State{availabilityv1alpha1.StateIdle, availabilityv1alpha1.StateActive} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			activityType := availabilityv1alpha1.ActivityGame
			if state == availabilityv1alpha1.StateIdle {
				activityType = availabilityv1alpha1.ActivityIdle
			}
			r, a, n, e := evictionFixture(t, state, activityType, corev1.TaintEffectNoSchedule, []*corev1.Pod{eligiblePod("standard")})
			r.Policy.BurstTaintKey = "example.com/burst"
			// Even custom equal taint values must not confuse an outage fence with a game.
			r.Policy.FailClosedValue = r.Policy.ActiveValue
			n.Status.Conditions[0].Status = corev1.ConditionUnknown
			if err := r.Status().Update(ctx, n); err != nil {
				t.Fatal(err)
			}
			n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: "node.kubernetes.io/unreachable", Effect: corev1.TaintEffectNoExecute})
			if err := r.Update(ctx, n); err != nil {
				t.Fatal(err)
			}
			req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: a.Name}}
			result, err := r.Reconcile(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			if result.RequeueAfter <= 0 {
				t.Fatal("outage requires requeue")
			}
			if err := r.Get(ctx, types.NamespacedName{Name: n.Name}, n); err != nil {
				t.Fatal(err)
			}
			if !hasTaint(n, "node.kubernetes.io/unreachable", "", corev1.TaintEffectNoExecute) {
				t.Fatal("native fencing changed")
			}
			if !hasTaint(n, r.Policy.Key, r.Policy.FailClosedValue, corev1.TaintEffectNoSchedule) || n.Labels[SchedulableNodeLabel] != "" {
				t.Fatal("node not fenced")
			}
			if len(e.names) != 0 {
				t.Fatal("outage must not authorize standard eviction")
			}
			if err := r.Get(ctx, req.NamespacedName, a); err != nil {
				t.Fatal(err)
			}
			if a.Status.Admission.Admitted || a.Status.Admission.Reason != "node-not-ready" || !a.Status.Admission.BurstTaint.Desired || !a.Status.Admission.BurstTaint.Applied {
				t.Fatalf("inconsistent admission: %+v", a.Status.Admission)
			}
			if testutil.ToFloat64(schedulableMetric.WithLabelValues(n.Name)) != 0 {
				t.Fatal("metric falsely admits node")
			}
		})
	}
}

func TestFinalizationPreservesUnavailableSiblingFence(t *testing.T) {
	ctx := context.Background()
	r, a, n, _ := evictionFixture(t, availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, corev1.TaintEffectNoSchedule, nil)
	r.Policy.BurstTaintKey = "example.com/burst"
	n.Status.Conditions[0].Status = corev1.ConditionFalse
	if err := r.Status().Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	// finalize recomputes from surviving enrollment a (the deleted object need not
	// exist in this client); it must not remove a surviving NotReady fence.
	deleted := a.DeepCopy()
	deleted.Name = "deleted-sibling"
	if err := r.finalize(ctx, deleted); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, types.NamespacedName{Name: n.Name}, n); err != nil {
		t.Fatal(err)
	}
	if !hasTaint(n, r.Policy.Key, r.Policy.FailClosedValue, corev1.TaintEffectNoSchedule) || !hasTaint(n, r.Policy.BurstTaintKey, r.Policy.FailClosedValue, corev1.TaintEffectNoSchedule) {
		t.Fatalf("sibling fencing removed: %+v", n.Spec.Taints)
	}
}

func TestReadinessFenceNeverCreatesLossyEvictionAuthority(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh idle", true: "stale fail-open"}[stale], func(t *testing.T) {
			ctx := context.Background()
			pod := eligiblePodWithLabels("lossy", map[string]string{EvictableLabel: "true", AllowLossyEvictionLabel: "true"})
			r, a, n, e := evictionFixture(t, availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, corev1.TaintEffectNoSchedule, []*corev1.Pod{pod})
			r.Policy.BurstTaintKey = "example.com/burst"
			if stale {
				r.Policy.FailClosed = false
				a.Status.HeartbeatAt = a.Status.HeartbeatAt.Add(-time.Hour)
				if err := r.Status().Update(ctx, a); err != nil {
					t.Fatal(err)
				}
			}
			n.Status.Conditions[0].Status = corev1.ConditionUnknown
			if err := r.Status().Update(ctx, n); err != nil {
				t.Fatal(err)
			}
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: a.Name}})
			if err != nil {
				t.Fatal(err)
			}
			if len(e.names) != 0 {
				t.Fatal("admission fence granted new lossy eviction authority")
			}
		})
	}
}

func TestReconcileRecoveryRequiresNewHeartbeat(t *testing.T) {
	ctx := context.Background()
	r, a, n, _ := evictionFixture(t, availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, corev1.TaintEffectNoSchedule, nil)
	r.Policy.BurstTaintKey = "example.com/burst"
	transition := a.Status.HeartbeatAt.Add(time.Second)
	n.Status.Conditions[0].LastTransitionTime = metav1.NewTime(transition)
	if err := r.Status().Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: a.Name}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, req.NamespacedName, a); err != nil {
		t.Fatal(err)
	}
	if a.Status.Admission.Admitted || a.Status.Admission.Reason != "node-recovery-pending" {
		t.Fatalf("premature recovery: %+v", a.Status.Admission)
	}
	a.Status.HeartbeatAt = transition
	if err := r.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, req.NamespacedName, a); err != nil {
		t.Fatal(err)
	}
	if !a.Status.Admission.Admitted || a.Status.Admission.BurstTaint.Desired || a.Status.Admission.BurstTaint.Applied {
		t.Fatalf("fresh recovery not admitted: %+v", a.Status.Admission)
	}
	if err := r.Get(ctx, types.NamespacedName{Name: n.Name}, n); err != nil {
		t.Fatal(err)
	}
	if n.Labels[SchedulableNodeLabel] != "true" || hasTaint(n, r.Policy.Key, r.Policy.FailClosedValue, corev1.TaintEffectNoSchedule) {
		t.Fatal("recovery fencing not cleared")
	}
}
