package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
)

func TestHeartbeatFreshnessBounds(t *testing.T) {
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	stale := time.Minute
	skew := DefaultHeartbeatSkew
	age := func(seconds int64) *int64 { return &seconds }

	tests := []struct {
		name        string
		at          time.Time
		now         time.Time
		skew        time.Duration
		wantFresh   bool
		wantMissing bool
		wantExpired bool
		wantFuture  bool
		wantAge     *int64
		wantLead    time.Duration
	}{
		{name: "current", at: now, now: now, skew: skew, wantFresh: true, wantAge: age(0)},
		{name: "tolerated skew", at: now.Add(skew), now: now, skew: skew, wantFresh: true, wantAge: age(0)},
		{name: "inside skew", at: now.Add(skew - time.Second), now: now, skew: skew, wantFresh: true, wantAge: age(0)},
		{name: "excessive future", at: now.Add(skew + time.Second), now: now, skew: skew, wantFuture: true, wantLead: skew + time.Second},
		{name: "missing", at: time.Time{}, now: now, skew: skew, wantMissing: true},
		{name: "just inside stale window", at: now.Add(-stale + time.Second), now: now, skew: skew, wantFresh: true, wantAge: age(59)},
		{name: "expired at staleAfter", at: now.Add(-stale), now: now, skew: skew, wantExpired: true, wantAge: age(60)},
		{name: "expired beyond staleAfter", at: now.Add(-2 * stale), now: now, skew: skew, wantExpired: true, wantAge: age(120)},
		{name: "clock stepped back within skew", at: now, now: now.Add(-skew), skew: skew, wantFresh: true, wantAge: age(0)},
		{name: "clock stepped back beyond skew", at: now, now: now.Add(-skew - time.Second), skew: skew, wantFuture: true, wantLead: skew + time.Second},
		{name: "clock stepped forward inside staleAfter", at: now, now: now.Add(stale - time.Second), skew: skew, wantFresh: true, wantAge: age(59)},
		{name: "clock stepped forward to staleAfter", at: now, now: now.Add(stale), skew: skew, wantExpired: true, wantAge: age(60)},
		{name: "custom skew tolerates its bound", at: now.Add(5 * time.Second), now: now, skew: 5 * time.Second, wantFresh: true, wantAge: age(0)},
		{name: "custom skew rejects a larger lead", at: now.Add(6 * time.Second), now: now, skew: 5 * time.Second, wantFuture: true, wantLead: 6 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := heartbeatFreshness(test.at, test.now, stale, test.skew)
			if got.Fresh != test.wantFresh || got.Missing != test.wantMissing || got.Expired != test.wantExpired || got.Future != test.wantFuture {
				t.Fatalf("verdict = %+v", got)
			}
			if test.wantAge == nil {
				if got.AgeSeconds != nil {
					t.Fatalf("age = %d, want nil", *got.AgeSeconds)
				}
			} else if got.AgeSeconds == nil || *got.AgeSeconds != *test.wantAge {
				t.Fatalf("age = %v, want %d", got.AgeSeconds, *test.wantAge)
			}
			if got.Lead != test.wantLead {
				t.Fatalf("lead = %s, want %s", got.Lead, test.wantLead)
			}
		})
	}

	if got := futureAgeSeconds(100 * time.Millisecond); got != -1 {
		t.Fatalf("sub-second lead age = %d, want -1", got)
	}
	if got := futureAgeSeconds(16*time.Second + time.Nanosecond); got != -17 {
		t.Fatalf("rounded lead age = %d, want -17", got)
	}
}

func TestHeartbeatSkewPolicyValidation(t *testing.T) {
	policy := testPolicy()
	if policy.HeartbeatSkew != 0 {
		t.Fatalf("test policy skew = %s, want the zero value so the default applies", policy.HeartbeatSkew)
	}
	if policy.effectiveHeartbeatSkew() != DefaultHeartbeatSkew {
		t.Fatalf("effective skew = %s", policy.effectiveHeartbeatSkew())
	}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	if !heartbeatFreshness(now.Add(time.Second), now, policy.StaleAfter, policy.effectiveHeartbeatSkew()).Fresh {
		t.Fatal("default skew rejected a one-second lead")
	}

	negative := policy
	negative.HeartbeatSkew = -time.Second
	if err := negative.Validate(); err == nil || !strings.Contains(err.Error(), "heartbeat-skew") {
		t.Fatalf("negative skew error = %v", err)
	}
	tooWide := policy
	tooWide.HeartbeatSkew = policy.StaleAfter
	if err := tooWide.Validate(); err == nil || !strings.Contains(err.Error(), "less than") {
		t.Fatalf("wide skew error = %v", err)
	}
}

func TestFutureHeartbeatFailsClosedAndRecovers(t *testing.T) {
	resetMetricsForTesting()
	defer resetMetricsForTesting()

	standard, lossy := contractPods()
	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, corev1.TaintEffectNoSchedule, []*corev1.Pod{standard, lossy})
	now := activity.Status.HeartbeatAt
	skew := r.Policy.effectiveHeartbeatSkew()
	key := types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}
	lead := skew + time.Minute
	writeActivityStatus(t, r, key, func(status *availabilityv1alpha1.NodeActivityStatus) {
		status.State = availabilityv1alpha1.StateIdle
		status.Activity = availabilityv1alpha1.ActivityIdle
		status.HeartbeatAt = now.Add(lead)
	})

	result := reconcileActivity(t, r, key)
	if result.RequeueAfter != time.Minute {
		t.Fatalf("requeue = %s, want %s", result.RequeueAfter, time.Minute)
	}
	gotNode, gotActivity := loadNodeActivity(t, r, node.Name, key)
	if !hasTaint(gotNode, r.Policy.Key, r.Policy.FailClosedValue, corev1.TaintEffectNoSchedule) {
		t.Fatalf("taints = %#v", gotNode.Spec.Taints)
	}
	if gotNode.Labels != nil && gotNode.Labels[SchedulableNodeLabel] != "" {
		t.Fatalf("schedulable label = %#v", gotNode.Labels)
	}
	if len(evictor.names) != 0 {
		t.Fatalf("future timestamp authorized eviction: %#v", evictor.names)
	}
	admission := gotActivity.Status.Admission
	if admission == nil || admission.Admitted || admission.Reason != admissionInvalidHeartbeat || admission.HeartbeatValid || admission.HeartbeatAgeSeconds != nil {
		t.Fatalf("admission = %#v", admission)
	}
	if admission.NextTransitionReason != transitionHeartbeat || admission.NextTransition == nil || !admission.NextTransition.Time.Equal(now.Add(time.Minute)) {
		t.Fatalf("next transition = %#v", admission)
	}
	condition := meta.FindStatusCondition(gotActivity.Status.Conditions, InvalidHeartbeatCondition)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != FutureHeartbeatReason {
		t.Fatalf("condition = %#v", condition)
	}
	if !strings.Contains(condition.Message, now.Add(lead).UTC().Format(time.RFC3339Nano)) || !strings.Contains(condition.Message, skew.String()) || !strings.Contains(condition.Message, "NTP") {
		t.Fatalf("message = %q", condition.Message)
	}
	assertGauge(t, "interactive_node_controller_heartbeat_valid", node.Name, "", "", 0)
	assertGauge(t, "interactive_node_controller_heartbeat_age_seconds", node.Name, "", "", float64(futureAgeSeconds(lead)))
	assertLabeledGauge(t, "interactive_node_controller_admission", node.Name, "reason", admissionInvalidHeartbeat, 1)

	writeActivityStatus(t, r, key, func(status *availabilityv1alpha1.NodeActivityStatus) {
		status.HeartbeatAt = now
	})
	reconcileActivity(t, r, key)
	gotNode, gotActivity = loadNodeActivity(t, r, node.Name, key)
	if len(gotNode.Spec.Taints) != 0 {
		t.Fatalf("recovered taints = %#v", gotNode.Spec.Taints)
	}
	if gotNode.Labels[SchedulableNodeLabel] != "true" {
		t.Fatalf("recovered labels = %#v", gotNode.Labels)
	}
	if len(evictor.names) != 0 {
		t.Fatalf("recovery evicted %#v", evictor.names)
	}
	admission = gotActivity.Status.Admission
	if admission == nil || !admission.Admitted || admission.Reason != admissionFreshIdle || !admission.HeartbeatValid || admission.HeartbeatAgeSeconds == nil || *admission.HeartbeatAgeSeconds != 0 {
		t.Fatalf("recovered admission = %#v", admission)
	}
	condition = meta.FindStatusCondition(gotActivity.Status.Conditions, InvalidHeartbeatCondition)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != HeartbeatTimeValidReason {
		t.Fatalf("recovered condition = %#v", condition)
	}
	assertGauge(t, "interactive_node_controller_heartbeat_valid", node.Name, "", "", 1)
	assertLabeledGauge(t, "interactive_node_controller_admission", node.Name, "reason", admissionFreshIdle, 1)
}

func TestHeartbeatClockAdjustments(t *testing.T) {
	resetMetricsForTesting()
	defer resetMetricsForTesting()

	standard, lossy := contractPods()
	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, corev1.TaintEffectNoSchedule, []*corev1.Pod{standard, lossy})
	now := activity.Status.HeartbeatAt
	clock := clocktesting.NewFakeClock(now)
	r.Clock = clock
	key := types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}
	skew := r.Policy.effectiveHeartbeatSkew()

	reconcileActivity(t, r, key)
	gotNode, _ := loadNodeActivity(t, r, node.Name, key)
	if gotNode.Labels[SchedulableNodeLabel] != "true" {
		t.Fatal("fresh idle was not admitted")
	}

	clock.SetTime(now.Add(-skew))
	reconcileActivity(t, r, key)
	gotNode, gotActivity := loadNodeActivity(t, r, node.Name, key)
	if gotNode.Labels[SchedulableNodeLabel] != "true" || gotActivity.Status.Admission == nil || !gotActivity.Status.Admission.HeartbeatValid {
		t.Fatalf("clock step inside skew closed admission: labels=%#v admission=%#v", gotNode.Labels, gotActivity.Status.Admission)
	}
	if condition := meta.FindStatusCondition(gotActivity.Status.Conditions, InvalidHeartbeatCondition); condition != nil && condition.Status == metav1.ConditionTrue {
		t.Fatalf("tolerated skew set %s", condition.Reason)
	}

	clock.SetTime(now.Add(-skew - time.Second))
	reconcileActivity(t, r, key)
	gotNode, gotActivity = loadNodeActivity(t, r, node.Name, key)
	if gotNode.Labels != nil && gotNode.Labels[SchedulableNodeLabel] != "" {
		t.Fatal("excessive clock step stayed admitted")
	}
	if !hasTaint(gotNode, r.Policy.Key, r.Policy.FailClosedValue, corev1.TaintEffectNoSchedule) {
		t.Fatalf("taints = %#v", gotNode.Spec.Taints)
	}
	if gotActivity.Status.Admission == nil || gotActivity.Status.Admission.Reason != admissionInvalidHeartbeat {
		t.Fatalf("admission = %#v", gotActivity.Status.Admission)
	}
	if len(evictor.names) != 0 {
		t.Fatalf("clock step authorized eviction: %#v", evictor.names)
	}

	clock.SetTime(now)
	reconcileActivity(t, r, key)
	gotNode, gotActivity = loadNodeActivity(t, r, node.Name, key)
	if gotNode.Labels[SchedulableNodeLabel] != "true" || len(gotNode.Spec.Taints) != 0 {
		t.Fatalf("clock recovery node = labels %#v taints %#v", gotNode.Labels, gotNode.Spec.Taints)
	}
	condition := meta.FindStatusCondition(gotActivity.Status.Conditions, InvalidHeartbeatCondition)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != HeartbeatTimeValidReason {
		t.Fatalf("clock recovery condition = %#v", condition)
	}

	clock.SetTime(now.Add(2 * r.Policy.StaleAfter))
	reconcileActivity(t, r, key)
	_, gotActivity = loadNodeActivity(t, r, node.Name, key)
	if gotActivity.Status.Admission == nil || gotActivity.Status.Admission.Reason != admissionStaleHeartbeat || gotActivity.Status.Admission.HeartbeatValid {
		t.Fatalf("forward clock admission = %#v", gotActivity.Status.Admission)
	}
	if condition = meta.FindStatusCondition(gotActivity.Status.Conditions, InvalidHeartbeatCondition); condition == nil || condition.Status != metav1.ConditionFalse {
		t.Fatalf("expired heartbeat raised InvalidHeartbeat: %#v", condition)
	}
	if len(evictor.names) != 1 || evictor.names[0].Name != lossy.Name {
		t.Fatalf("forward clock evictions = %#v", evictor.names)
	}
}

func TestConfiguredHeartbeatSkew(t *testing.T) {
	r, activity, node, _ := evictionFixture(t, availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, corev1.TaintEffectNoSchedule, nil)
	r.Policy.HeartbeatSkew = 5 * time.Second
	now := activity.Status.HeartbeatAt
	key := types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}

	writeActivityStatus(t, r, key, func(status *availabilityv1alpha1.NodeActivityStatus) {
		status.HeartbeatAt = now.Add(5 * time.Second)
	})
	reconcileActivity(t, r, key)
	gotNode, gotActivity := loadNodeActivity(t, r, node.Name, key)
	if gotNode.Labels[SchedulableNodeLabel] != "true" || !gotActivity.Status.Admission.HeartbeatValid {
		t.Fatalf("5s lead with 5s skew: labels=%#v admission=%#v", gotNode.Labels, gotActivity.Status.Admission)
	}

	writeActivityStatus(t, r, key, func(status *availabilityv1alpha1.NodeActivityStatus) {
		status.HeartbeatAt = now.Add(6 * time.Second)
	})
	reconcileActivity(t, r, key)
	gotNode, gotActivity = loadNodeActivity(t, r, node.Name, key)
	if gotNode.Labels != nil && gotNode.Labels[SchedulableNodeLabel] != "" {
		t.Fatal("6s lead stayed admitted")
	}
	if gotActivity.Status.Admission == nil || gotActivity.Status.Admission.Reason != admissionInvalidHeartbeat || gotActivity.Status.Admission.HeartbeatAgeSeconds != nil {
		t.Fatalf("admission = %#v", gotActivity.Status.Admission)
	}
}

func TestMissingAndExpiredHeartbeatsStillEvictLossy(t *testing.T) {
	for _, test := range []struct {
		name   string
		at     func(now time.Time) time.Time
		reason string
		age    *int64
	}{
		{name: "missing", at: func(time.Time) time.Time { return time.Time{} }, reason: admissionMissingHeartbeat},
		{name: "expired", at: func(now time.Time) time.Time { return now.Add(-2 * time.Minute) }, reason: admissionStaleHeartbeat, age: func() *int64 { seconds := int64(120); return &seconds }()},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetMetricsForTesting()
			defer resetMetricsForTesting()
			standard, lossy := contractPods()
			r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, corev1.TaintEffectNoSchedule, []*corev1.Pod{standard, lossy})
			key := types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}
			writeActivityStatus(t, r, key, func(status *availabilityv1alpha1.NodeActivityStatus) {
				status.HeartbeatAt = test.at(activity.Status.HeartbeatAt)
			})
			reconcileActivity(t, r, key)
			_, gotActivity := loadNodeActivity(t, r, node.Name, key)
			if gotActivity.Status.Admission == nil || gotActivity.Status.Admission.Reason != test.reason || gotActivity.Status.Admission.HeartbeatValid {
				t.Fatalf("admission = %#v", gotActivity.Status.Admission)
			}
			if test.age == nil {
				if gotActivity.Status.Admission.HeartbeatAgeSeconds != nil {
					t.Fatalf("age = %d", *gotActivity.Status.Admission.HeartbeatAgeSeconds)
				}
			} else if gotActivity.Status.Admission.HeartbeatAgeSeconds == nil || *gotActivity.Status.Admission.HeartbeatAgeSeconds != *test.age {
				t.Fatalf("age = %v, want %d", gotActivity.Status.Admission.HeartbeatAgeSeconds, *test.age)
			}
			if condition := meta.FindStatusCondition(gotActivity.Status.Conditions, InvalidHeartbeatCondition); condition != nil && condition.Status == metav1.ConditionTrue {
				t.Fatalf("condition = %#v", condition)
			}
			if len(evictor.names) != 1 || evictor.names[0].Name != lossy.Name {
				t.Fatalf("evictions = %#v, want only %s", evictor.names, lossy.Name)
			}
		})
	}
}

func TestEnrollmentAggregationUsesHeartbeatFreshness(t *testing.T) {
	t.Run("future game does not outrank as a fresh game", func(t *testing.T) {
		r, idle, node, evictor := evictionFixture(t, availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, corev1.TaintEffectNoSchedule, []*corev1.Pod{eligiblePod("candidate")})
		now := idle.Status.HeartbeatAt
		r.Clock = clocktesting.NewFakeClock(now)
		future := &availabilityv1alpha1.NodeActivity{ObjectMeta: metav1.ObjectMeta{Name: "future-game", Namespace: "other"}, Spec: idle.Spec}
		if err := r.Create(context.Background(), future); err != nil {
			t.Fatal(err)
		}
		future.Status = availabilityv1alpha1.NodeActivityStatus{
			State: availabilityv1alpha1.StateActive, Activity: availabilityv1alpha1.ActivityGame,
			HeartbeatAt: now.Add(r.Policy.effectiveHeartbeatSkew() + time.Minute),
		}
		if err := r.Status().Update(context.Background(), future); err != nil {
			t.Fatal(err)
		}
		reconcileActivity(t, r, types.NamespacedName{Name: idle.Name, Namespace: idle.Namespace})
		gotNode, gotIdle := loadNodeActivity(t, r, node.Name, types.NamespacedName{Name: idle.Name, Namespace: idle.Namespace})
		if hasTaint(gotNode, r.Policy.Key, r.Policy.ActiveValue, corev1.TaintEffectNoSchedule) {
			t.Fatal("future game was aggregated as a fresh game")
		}
		if !hasTaint(gotNode, r.Policy.Key, r.Policy.FailClosedValue, corev1.TaintEffectNoSchedule) {
			t.Fatalf("taints = %#v", gotNode.Spec.Taints)
		}
		if gotIdle.Status.Admission == nil || gotIdle.Status.Admission.Reason != admissionInvalidHeartbeat {
			t.Fatalf("admission = %#v", gotIdle.Status.Admission)
		}
		if len(evictor.names) != 0 {
			t.Fatalf("evictions = %#v", evictor.names)
		}
		reconcileActivity(t, r, types.NamespacedName{Name: future.Name, Namespace: future.Namespace})
		_, gotFuture := loadNodeActivity(t, r, node.Name, types.NamespacedName{Name: future.Name, Namespace: future.Namespace})
		condition := meta.FindStatusCondition(gotFuture.Status.Conditions, InvalidHeartbeatCondition)
		if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != FutureHeartbeatReason {
			t.Fatalf("future condition = %#v", condition)
		}
	})

	t.Run("fresh game outranks a future idle heartbeat", func(t *testing.T) {
		resetMetricsForTesting()
		defer resetMetricsForTesting()
		r, active, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{eligiblePod("candidate")})
		now := active.Status.HeartbeatAt
		r.Clock = clocktesting.NewFakeClock(now)
		future := &availabilityv1alpha1.NodeActivity{ObjectMeta: metav1.ObjectMeta{Name: "future-idle", Namespace: "other"}, Spec: active.Spec}
		if err := r.Create(context.Background(), future); err != nil {
			t.Fatal(err)
		}
		future.Status = availabilityv1alpha1.NodeActivityStatus{
			State: availabilityv1alpha1.StateIdle, Activity: availabilityv1alpha1.ActivityIdle,
			HeartbeatAt: now.Add(r.Policy.effectiveHeartbeatSkew() + time.Minute),
		}
		if err := r.Status().Update(context.Background(), future); err != nil {
			t.Fatal(err)
		}
		reconcileActivity(t, r, types.NamespacedName{Name: active.Name, Namespace: active.Namespace})
		gotNode, gotActive := loadNodeActivity(t, r, node.Name, types.NamespacedName{Name: active.Name, Namespace: active.Namespace})
		if !hasTaint(gotNode, r.Policy.Key, r.Policy.ActiveValue, corev1.TaintEffectNoSchedule) {
			t.Fatalf("taints = %#v", gotNode.Spec.Taints)
		}
		if gotActive.Status.Admission == nil || gotActive.Status.Admission.Reason != admissionActiveGame || !gotActive.Status.Admission.HeartbeatValid {
			t.Fatalf("admission = %#v", gotActive.Status.Admission)
		}
		if condition := meta.FindStatusCondition(gotActive.Status.Conditions, InvalidHeartbeatCondition); condition != nil && condition.Status == metav1.ConditionTrue {
			t.Fatalf("fresh game condition = %#v", condition)
		}
		if len(evictor.names) != 0 {
			t.Fatalf("duplicate enrollment evicted %#v", evictor.names)
		}
		reconcileActivity(t, r, types.NamespacedName{Name: future.Name, Namespace: future.Namespace})
		_, gotFuture := loadNodeActivity(t, r, node.Name, types.NamespacedName{Name: future.Name, Namespace: future.Namespace})
		condition := meta.FindStatusCondition(gotFuture.Status.Conditions, InvalidHeartbeatCondition)
		if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != FutureHeartbeatReason {
			t.Fatalf("future idle condition = %#v", condition)
		}
		assertGauge(t, "interactive_node_controller_heartbeat_valid", node.Name, "", "", 1)
	})
}

func writeActivityStatus(t *testing.T, r *NodeActivityReconciler, name types.NamespacedName, mutate func(*availabilityv1alpha1.NodeActivityStatus)) {
	t.Helper()
	current := &availabilityv1alpha1.NodeActivity{}
	if err := r.Get(context.Background(), name, current); err != nil {
		t.Fatal(err)
	}
	mutate(&current.Status)
	if err := r.Status().Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
}

func reconcileActivity(t *testing.T, r *NodeActivityReconciler, name types.NamespacedName) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: name})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func loadNodeActivity(t *testing.T, r *NodeActivityReconciler, nodeName string, name types.NamespacedName) (*corev1.Node, *availabilityv1alpha1.NodeActivity) {
	t.Helper()
	gotNode := &corev1.Node{}
	gotActivity := &availabilityv1alpha1.NodeActivity{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: nodeName}, gotNode); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), name, gotActivity); err != nil {
		t.Fatal(err)
	}
	return gotNode, gotActivity
}
