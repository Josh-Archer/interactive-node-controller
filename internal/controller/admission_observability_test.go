package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

func TestAdmissionSignalDoesNotClaimArbitraryScheduling(t *testing.T) {
	resetMetricsForTesting()
	defer resetMetricsForTesting()

	observeActivityAndTaint("signal-node", availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, nil, true)
	observeAdmission("signal-node", time.Now(), time.Now(), time.Minute, DefaultHeartbeatSkew, admissionFreshIdle, false, false, time.Time{})
	for _, name := range []string{"interactive_node_controller_schedulable", "interactive_node_controller_admission"} {
		help := metricHelp(t, name)
		if !strings.Contains(help, "arbitrary workload") {
			t.Fatalf("%s help = %q", name, help)
		}
	}
	if !strings.Contains(admissionSignalHelp, "arbitrary workload") || !strings.Contains(admissionDisclaimer, "arbitrary workload") {
		t.Fatal("admission help constants lost the scheduling disclaimer")
	}
}

func TestEvictionConditionDistinguishesOutcomes(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		r, activity, node, _ := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{eligiblePod("candidate")})
		r.Eviction.Enabled = false
		r.Eviction.Audit = false
		summary := mustEvict(t, r, activity, node, &node.Spec.Taints[0])
		assertEvictionCondition(t, summary, EvictionReasonDisabled, metav1.ConditionTrue)
	})

	t.Run("inactive policy", func(t *testing.T) {
		r, activity, node, _ := evictionFixture(t, availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, corev1.TaintEffectNoSchedule, []*corev1.Pod{eligiblePod("candidate")})
		summary := mustEvict(t, r, activity, node, nil)
		assertEvictionCondition(t, summary, EvictionReasonInactive, metav1.ConditionTrue)
		if strings.Contains(summary.condition(r.clock().Now(), 1).Reason, "Disabled") {
			t.Fatal("inactive policy reported eviction disabled")
		}
	})

	t.Run("no eligible pods", func(t *testing.T) {
		r, activity, node, _ := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, nil)
		summary := mustEvict(t, r, activity, node, &node.Spec.Taints[0])
		assertEvictionCondition(t, summary, EvictionReasonNoEligible, metav1.ConditionTrue)
		if summary.outcome != evictionStateNoEligible {
			t.Fatalf("outcome = %q", summary.outcome)
		}
	})

	t.Run("audit only", func(t *testing.T) {
		r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{eligiblePod("candidate")})
		r.Eviction.Enabled = false
		r.Eviction.Audit = true
		summary := mustEvict(t, r, activity, node, &node.Spec.Taints[0])
		assertEvictionCondition(t, summary, EvictionReasonAuditOnly, metav1.ConditionTrue)
		if len(evictor.names) != 0 {
			t.Fatalf("audit called the Eviction API: %#v", evictor.names)
		}
	})

	t.Run("completed", func(t *testing.T) {
		r, activity, node, _ := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{eligiblePod("candidate")})
		summary := mustEvict(t, r, activity, node, &node.Spec.Taints[0])
		assertEvictionCondition(t, summary, EvictionReasonCompleted, metav1.ConditionTrue)
	})

	t.Run("budget waiting", func(t *testing.T) {
		r, activity, node, _ := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{eligiblePod("one"), eligiblePod("two")})
		summary := mustEvict(t, r, activity, node, &node.Spec.Taints[0])
		assertEvictionCondition(t, summary, EvictionReasonBudgetWaiting, metav1.ConditionTrue)
		if summary.evicted != 1 || summary.deferred != 1 {
			t.Fatalf("summary = %#v", summary)
		}
	})

	t.Run("blocked", func(t *testing.T) {
		r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{eligiblePod("candidate")})
		evictor.err = apierrors.NewTooManyRequests("pdb", 0)
		summary := mustEvict(t, r, activity, node, &node.Spec.Taints[0])
		condition := assertEvictionCondition(t, summary, EvictionReasonBlocked, metav1.ConditionFalse)
		if !strings.Contains(condition.Message, "pdb-blocked") {
			t.Fatalf("message = %q", condition.Message)
		}
	})
}

func TestEvictionBlockerReasonsStayBounded(t *testing.T) {
	if got := boundedEvictionReason("workloads/candidate"); got != "unclassified" {
		t.Fatalf("counter reason = %q", got)
	}
	if got := boundedBlockerReason("workloads/candidate"); got != "api-error" {
		t.Fatalf("blocker reason = %q", got)
	}

	resetMetricsForTesting()
	defer resetMetricsForTesting()

	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	pod := eligiblePod("candidate")
	activity := &availabilityv1alpha1.NodeActivity{
		ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "availability"},
		Spec:       availabilityv1alpha1.NodeActivitySpec{NodeName: "desktop"},
		Status: availabilityv1alpha1.NodeActivityStatus{
			State: availabilityv1alpha1.StateActive, Activity: availabilityv1alpha1.ActivityGame, HeartbeatAt: now,
		},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "desktop"}}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = availabilityv1alpha1.AddToScheme(scheme)
	api := indexNodeActivities(fake.NewClientBuilder().WithScheme(scheme)).
		WithIndex(&corev1.Pod{}, PodNodeNameKey, IndexPodNodeName).
		WithStatusSubresource(activity).
		WithObjects(activity, node, pod).
		Build()
	evictor := &recordingEvictor{err: apierrors.NewTooManyRequests("pdb", 0)}
	policy := testPolicy()
	reconciler := &NodeActivityReconciler{
		Client: api, Clock: clocktesting.NewFakeClock(now), Policy: policy,
		Eviction: EvictionPolicy{Enabled: true, Audit: false, ProtectedNamespaces: map[string]struct{}{"kube-system": {}}, MaxPerReconcile: 1, RetryBackoff: time.Minute},
		Evictor:  evictor,
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	updated := &availabilityv1alpha1.NodeActivity{}
	if err := api.Get(context.Background(), request.NamespacedName, updated); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(updated.Status.Conditions, EvictionCondition)
	if condition == nil || condition.Reason != EvictionReasonBlocked || !strings.Contains(condition.Message, pod.Name) {
		t.Fatalf("condition = %#v", condition)
	}
	blocked := samplesForNode(t, "interactive_node_controller_eviction_blocked", node.Name)
	if len(blocked) != 1 || blocked[0].labels["reason"] != "pdb-blocked" || blocked[0].value != 1 {
		t.Fatalf("blocked series = %#v", blocked)
	}
	for _, sample := range blocked {
		for _, value := range sample.labels {
			if strings.Contains(value, "/") || strings.Contains(value, pod.Name) {
				t.Fatalf("blocker label is unbounded: %#v", sample.labels)
			}
		}
	}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	blocked = samplesForNode(t, "interactive_node_controller_eviction_blocked", node.Name)
	if len(blocked) != 1 || blocked[0].labels["reason"] != "retry-backoff" {
		t.Fatalf("retry series = %#v", blocked)
	}
	if len(evictor.names) != 1 {
		t.Fatalf("eviction attempts = %#v", evictor.names)
	}
}

func TestAdmissionStatusMatchesNodeAndMetrics(t *testing.T) {
	resetMetricsForTesting()
	defer resetMetricsForTesting()

	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	nodeName := "workstation-signals"
	policy := testPolicy()
	policy.BurstTaintKey = "availability.interactive-node.io/burst"
	activity := &availabilityv1alpha1.NodeActivity{
		ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "availability"},
		Spec:       availabilityv1alpha1.NodeActivitySpec{NodeName: nodeName},
		Status: availabilityv1alpha1.NodeActivityStatus{
			State: availabilityv1alpha1.StateIdle, Activity: availabilityv1alpha1.ActivityIdle,
		},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = availabilityv1alpha1.AddToScheme(scheme)
	api := indexNodeActivities(fake.NewClientBuilder().WithScheme(scheme)).
		WithIndex(&corev1.Pod{}, PodNodeNameKey, IndexPodNodeName).
		WithStatusSubresource(activity).
		WithObjects(activity, node).
		Build()
	reconciler := &NodeActivityReconciler{
		Client: api, Clock: clocktesting.NewFakeClock(now), Policy: policy,
		Eviction: EvictionPolicy{Enabled: true, Audit: false, MaxPerReconcile: 1, RetryBackoff: time.Second},
		Evictor:  &recordingEvictor{},
	}
	key := types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}
	request := ctrl.Request{NamespacedName: key}

	reconcile := func() {
		t.Helper()
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	load := func() (*corev1.Node, *availabilityv1alpha1.NodeActivity) {
		t.Helper()
		gotNode := &corev1.Node{}
		gotActivity := &availabilityv1alpha1.NodeActivity{}
		if err := api.Get(context.Background(), types.NamespacedName{Name: nodeName}, gotNode); err != nil {
			t.Fatal(err)
		}
		if err := api.Get(context.Background(), key, gotActivity); err != nil {
			t.Fatal(err)
		}
		return gotNode, gotActivity
	}
	writeStatus := func(mutate func(*availabilityv1alpha1.NodeActivity)) {
		t.Helper()
		current := &availabilityv1alpha1.NodeActivity{}
		if err := api.Get(context.Background(), key, current); err != nil {
			t.Fatal(err)
		}
		mutate(current)
		if err := api.Status().Update(context.Background(), current); err != nil {
			t.Fatal(err)
		}
	}

	reconcile()
	gotNode, gotActivity := load()
	assertSignalsMatch(t, gotNode, gotActivity, policy, admissionMissingHeartbeat, false, EvictionReasonNoEligible)
	if gotActivity.Status.Admission.HeartbeatAgeSeconds != nil || gotActivity.Status.Admission.HeartbeatValid {
		t.Fatalf("missing heartbeat admission = %#v", gotActivity.Status.Admission)
	}
	if len(samplesForNode(t, "interactive_node_controller_heartbeat_age_seconds", nodeName)) != 0 {
		t.Fatal("missing heartbeat published an age series")
	}

	writeStatus(func(activity *availabilityv1alpha1.NodeActivity) {
		activity.Status.State = availabilityv1alpha1.StateActive
		activity.Status.Activity = availabilityv1alpha1.ActivityGame
		activity.Status.HeartbeatAt = now
	})
	reconcile()
	gotNode, gotActivity = load()
	assertSignalsMatch(t, gotNode, gotActivity, policy, admissionActiveGame, false, EvictionReasonNoEligible)
	if age := gotActivity.Status.Admission.HeartbeatAgeSeconds; age == nil || *age != 0 || !gotActivity.Status.Admission.HeartbeatValid {
		t.Fatalf("fresh heartbeat = %#v", gotActivity.Status.Admission)
	}
	wantTransition := now.Add(policy.StaleAfter)
	if gotActivity.Status.Admission.NextTransitionReason != transitionHeartbeat || gotActivity.Status.Admission.NextTransition == nil || !gotActivity.Status.Admission.NextTransition.Time.Equal(wantTransition) {
		t.Fatalf("next transition = %#v", gotActivity.Status.Admission)
	}
	if got := samplesForNode(t, "interactive_node_controller_next_transition_timestamp_seconds", nodeName); len(got) != 1 || got[0].value != float64(wantTransition.Unix()) {
		t.Fatalf("transition metric = %#v", got)
	}
	if len(samplesForNode(t, "interactive_node_controller_admission", nodeName)) != 1 {
		t.Fatal("admission reason series leaked across the state change")
	}

	writeStatus(func(activity *availabilityv1alpha1.NodeActivity) {
		activity.Status.State = availabilityv1alpha1.StateIdle
		activity.Status.Activity = availabilityv1alpha1.ActivityIdle
		activity.Status.HeartbeatAt = now
	})
	reconcile()
	gotNode, gotActivity = load()
	assertSignalsMatch(t, gotNode, gotActivity, policy, admissionFreshIdle, true, EvictionReasonInactive)
	if len(gotNode.Spec.Taints) != 0 {
		t.Fatalf("fresh idle taints = %#v", gotNode.Spec.Taints)
	}

	writeStatus(func(activity *availabilityv1alpha1.NodeActivity) {
		activity.Status.HeartbeatAt = now.Add(-2 * time.Minute)
	})
	reconcile()
	gotNode, gotActivity = load()
	assertSignalsMatch(t, gotNode, gotActivity, policy, admissionStaleHeartbeat, false, EvictionReasonNoEligible)
	if age := gotActivity.Status.Admission.HeartbeatAgeSeconds; age == nil || *age != 120 || gotActivity.Status.Admission.HeartbeatValid {
		t.Fatalf("stale heartbeat = %#v", gotActivity.Status.Admission)
	}

	writeStatus(func(activity *availabilityv1alpha1.NodeActivity) {
		activity.Status.HeartbeatAt = now
	})
	current := &availabilityv1alpha1.NodeActivity{}
	if err := api.Get(context.Background(), key, current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Windows = []availabilityv1alpha1.AvailabilityWindow{{Schedule: "not a cron", Duration: "1h", State: "game"}}
	if err := api.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	reconcile()
	gotNode, gotActivity = load()
	assertSignalsMatch(t, gotNode, gotActivity, policy, admissionInvalidSchedule, false, EvictionReasonNoEligible)
	invalid := meta.FindStatusCondition(gotActivity.Status.Conditions, InvalidScheduleCondition)
	if invalid == nil || invalid.Status != metav1.ConditionTrue {
		t.Fatalf("invalid schedule = %#v", invalid)
	}
	if got := samplesForNode(t, "interactive_node_controller_config_invalid", nodeName); len(got) != 1 || got[0].labels["reason"] != "schedule" || got[0].value != 1 {
		t.Fatalf("config metric = %#v", got)
	}

	current = &availabilityv1alpha1.NodeActivity{}
	if err := api.Get(context.Background(), key, current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Windows = nil
	if err := api.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	reconcile()
	gotNode, gotActivity = load()
	assertSignalsMatch(t, gotNode, gotActivity, policy, admissionFreshIdle, true, EvictionReasonInactive)
	if len(samplesForNode(t, "interactive_node_controller_config_invalid", nodeName)) != 0 {
		t.Fatal("config metric remained after the schedule was fixed")
	}

	if err := api.Delete(context.Background(), gotActivity); err != nil {
		t.Fatal(err)
	}
	reconcile()
	assertNodeSeriesCleared(t, nodeName)
}

func TestNextTransitionReusesWindowTelemetry(t *testing.T) {
	resetMetricsForTesting()
	defer resetMetricsForTesting()

	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	nodeName := "workstation-window"
	policy := testPolicy()
	policy.StaleAfter = 24 * time.Hour
	activity := &availabilityv1alpha1.NodeActivity{
		ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "availability"},
		Spec: availabilityv1alpha1.NodeActivitySpec{
			NodeName: nodeName,
			Windows:  []availabilityv1alpha1.AvailabilityWindow{{Schedule: "30 20 * * *", Duration: "1h", TimeZone: "UTC", State: "game"}},
		},
		Status: availabilityv1alpha1.NodeActivityStatus{
			State: availabilityv1alpha1.StateIdle, Activity: availabilityv1alpha1.ActivityIdle, HeartbeatAt: now,
		},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = availabilityv1alpha1.AddToScheme(scheme)
	api := indexNodeActivities(fake.NewClientBuilder().WithScheme(scheme)).WithStatusSubresource(activity).WithObjects(activity, node).Build()
	reconciler := &NodeActivityReconciler{Client: api, Clock: clocktesting.NewFakeClock(now), Policy: policy}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}}); err != nil {
		t.Fatal(err)
	}
	_, nextWindow, _, err := evaluateWindows(activity.Spec, now)
	if err != nil {
		t.Fatal(err)
	}
	got := &availabilityv1alpha1.NodeActivity{}
	if err := api.Get(context.Background(), types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Admission == nil || got.Status.Admission.Reason != admissionFreshIdle || !got.Status.Admission.Admitted {
		t.Fatalf("admission = %#v", got.Status.Admission)
	}
	if got.Status.Admission.NextTransitionReason != transitionWindow || got.Status.Admission.NextTransition == nil || !got.Status.Admission.NextTransition.Time.Equal(nextWindow) {
		t.Fatalf("next transition = %#v want %s", got.Status.Admission, nextWindow)
	}
	windowMetric := samplesForNode(t, "inc_next_window_start_timestamp_seconds", nodeName)
	transitionMetric := samplesForNode(t, "interactive_node_controller_next_transition_timestamp_seconds", nodeName)
	if len(windowMetric) != 1 || len(transitionMetric) != 1 || windowMetric[0].value != transitionMetric[0].value || windowMetric[0].value != float64(nextWindow.Unix()) {
		t.Fatalf("window %#v transition %#v", windowMetric, transitionMetric)
	}
	if active := samplesForNode(t, "inc_window_active", nodeName); len(active) != 1 || active[0].value != 0 {
		t.Fatalf("window active = %#v", active)
	}
}

func TestHoldIsTheNextPolicyTransition(t *testing.T) {
	resetMetricsForTesting()
	defer resetMetricsForTesting()

	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	nodeName := "workstation-hold"
	policy := testPolicy()
	policy.StaleAfter = 2 * time.Hour
	activity := &availabilityv1alpha1.NodeActivity{
		ObjectMeta: metav1.ObjectMeta{
			Name: "desktop", Namespace: "availability",
			Annotations: map[string]string{HoldAnnotation: "30m"},
		},
		Spec: availabilityv1alpha1.NodeActivitySpec{NodeName: nodeName},
		Status: availabilityv1alpha1.NodeActivityStatus{
			State: availabilityv1alpha1.StateIdle, Activity: availabilityv1alpha1.ActivityIdle, HeartbeatAt: now,
		},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = availabilityv1alpha1.AddToScheme(scheme)
	api := indexNodeActivities(fake.NewClientBuilder().WithScheme(scheme)).WithStatusSubresource(activity).WithObjects(activity, node).Build()
	reconciler := &NodeActivityReconciler{Client: api, Clock: clocktesting.NewFakeClock(now), Policy: policy}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}}); err != nil {
		t.Fatal(err)
	}
	gotNode := &corev1.Node{}
	gotActivity := &availabilityv1alpha1.NodeActivity{}
	if err := api.Get(context.Background(), types.NamespacedName{Name: nodeName}, gotNode); err != nil {
		t.Fatal(err)
	}
	if err := api.Get(context.Background(), types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}, gotActivity); err != nil {
		t.Fatal(err)
	}
	assertSignalsMatch(t, gotNode, gotActivity, policy, admissionHold, false, EvictionReasonDisabled)
	if gotActivity.Status.Admission.NextTransitionReason != transitionHold || gotActivity.Status.Admission.NextTransition == nil {
		t.Fatalf("hold transition = %#v", gotActivity.Status.Admission)
	}
	if !gotActivity.Status.Admission.NextTransition.Time.Equal(now.Add(30 * time.Minute)) {
		t.Fatalf("hold expiry = %s", gotActivity.Status.Admission.NextTransition.Time)
	}
	if len(gotNode.Spec.Taints) != 1 || gotNode.Spec.Taints[0].Value != policy.InteractiveValue || gotNode.Spec.Taints[0].Effect != corev1.TaintEffectPreferNoSchedule {
		t.Fatalf("hold taint = %#v", gotNode.Spec.Taints)
	}
}

func TestMissingNodeClearsSignals(t *testing.T) {
	resetMetricsForTesting()
	defer resetMetricsForTesting()

	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	nodeName := "workstation-missing"
	policy := testPolicy()
	policy.BurstTaintKey = "availability.interactive-node.io/burst"
	activity := &availabilityv1alpha1.NodeActivity{
		ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "availability"},
		Spec:       availabilityv1alpha1.NodeActivitySpec{NodeName: nodeName},
		Status: availabilityv1alpha1.NodeActivityStatus{
			State: availabilityv1alpha1.StateActive, Activity: availabilityv1alpha1.ActivityGame, HeartbeatAt: now,
		},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = availabilityv1alpha1.AddToScheme(scheme)
	api := indexNodeActivities(fake.NewClientBuilder().WithScheme(scheme)).
		WithIndex(&corev1.Pod{}, PodNodeNameKey, IndexPodNodeName).
		WithStatusSubresource(activity).
		WithObjects(activity, node).
		Build()
	reconciler := &NodeActivityReconciler{
		Client: api, Clock: clocktesting.NewFakeClock(now), Policy: policy,
		Eviction: EvictionPolicy{Enabled: true, Audit: false, MaxPerReconcile: 1, RetryBackoff: time.Second},
		Evictor:  &recordingEvictor{},
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(samplesForNode(t, "interactive_node_controller_admission", nodeName)) != 1 {
		t.Fatal("expected admission series before the node disappeared")
	}
	if err := api.Delete(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	assertNodeSeriesCleared(t, nodeName)
	got := &availabilityv1alpha1.NodeActivity{}
	if err := api.Get(context.Background(), request.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Admission == nil || got.Status.Admission.Reason != admissionNodeNotFound || got.Status.Admission.Admitted || got.Status.Admission.BurstTaint.Applied {
		t.Fatalf("admission = %#v", got.Status.Admission)
	}
	eviction := meta.FindStatusCondition(got.Status.Conditions, EvictionCondition)
	if eviction == nil || eviction.Reason != EvictionReasonInactive {
		t.Fatalf("eviction = %#v", eviction)
	}
}

func mustEvict(t *testing.T, r *NodeActivityReconciler, activity *availabilityv1alpha1.NodeActivity, node *corev1.Node, desired *corev1.Taint) evictionSummary {
	t.Helper()
	summary, err := r.reconcileEvictions(context.Background(), activity, node, desired)
	if err != nil {
		t.Fatal(err)
	}
	return summary
}

func assertEvictionCondition(t *testing.T, summary evictionSummary, reason string, status metav1.ConditionStatus) metav1.Condition {
	t.Helper()
	condition := summary.condition(time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC), 1)
	if condition.Reason != reason || condition.Status != status || condition.Message == "" {
		t.Fatalf("condition = %#v, want %s/%s", condition, status, reason)
	}
	return condition
}

func assertSignalsMatch(t *testing.T, node *corev1.Node, activity *availabilityv1alpha1.NodeActivity, policy TaintPolicy, reason string, admitted bool, evictionReason string) {
	t.Helper()
	admission := activity.Status.Admission
	if admission == nil {
		t.Fatal("admission status missing")
	}
	if admission.Admitted != admitted || admission.Reason != reason {
		t.Fatalf("admission = %#v, want admitted=%v reason=%s", admission, admitted, reason)
	}
	labeled := node.Labels != nil && node.Labels[SchedulableNodeLabel] == "true"
	if labeled != admitted {
		t.Fatalf("schedulable label = %v, admitted = %v", labeled, admitted)
	}
	var primary *corev1.Taint
	for i := range node.Spec.Taints {
		if node.Spec.Taints[i].Key == policy.Key {
			primary = &node.Spec.Taints[i]
			break
		}
	}
	switch {
	case activity.Status.ManagedTaint == nil && primary != nil:
		t.Fatalf("node taint %#v has no status.managedTaint", primary)
	case activity.Status.ManagedTaint != nil && primary == nil:
		t.Fatalf("status taint %#v is absent on the node", activity.Status.ManagedTaint)
	case activity.Status.ManagedTaint != nil && (primary.Value != activity.Status.ManagedTaint.Value || string(primary.Effect) != activity.Status.ManagedTaint.Effect):
		t.Fatalf("node taint %#v status %#v", primary, activity.Status.ManagedTaint)
	}
	burstApplied := policy.BurstTaintKey != "" && hasTaint(node, policy.BurstTaintKey, policy.FailClosedValue, corev1.TaintEffectNoSchedule)
	if admission.BurstTaint.Configured != (policy.BurstTaintKey != "") || admission.BurstTaint.Applied != burstApplied || admission.BurstTaint.Desired != (policy.BurstTaintKey != "" && !admitted) {
		t.Fatalf("burst status %#v applied %v", admission.BurstTaint, burstApplied)
	}
	eviction := meta.FindStatusCondition(activity.Status.Conditions, EvictionCondition)
	if eviction == nil || eviction.Reason != evictionReason {
		t.Fatalf("eviction = %#v, want %s", eviction, evictionReason)
	}
	assertGauge(t, "interactive_node_controller_schedulable", node.Name, "", "", boolGauge(admitted))
	assertLabeledGauge(t, "interactive_node_controller_admission", node.Name, "reason", reason, 1)
	if len(samplesForNode(t, "interactive_node_controller_admission", node.Name)) != 1 {
		t.Fatalf("admission series = %#v", samplesForNode(t, "interactive_node_controller_admission", node.Name))
	}
	assertGauge(t, "interactive_node_controller_heartbeat_valid", node.Name, "", "", boolGauge(admission.HeartbeatValid))
	ageSamples := samplesForNode(t, "interactive_node_controller_heartbeat_age_seconds", node.Name)
	if admission.HeartbeatAgeSeconds == nil {
		if len(ageSamples) != 0 {
			t.Fatalf("age series = %#v", ageSamples)
		}
	} else if len(ageSamples) != 1 || ageSamples[0].value != float64(*admission.HeartbeatAgeSeconds) {
		t.Fatalf("age series %#v status %v", ageSamples, admission.HeartbeatAgeSeconds)
	}
	burstSamples := samplesForNode(t, "interactive_node_controller_burst_taint", node.Name)
	if policy.BurstTaintKey == "" {
		if len(burstSamples) != 0 {
			t.Fatalf("burst series = %#v", burstSamples)
		}
	} else if len(burstSamples) != 1 || burstSamples[0].value != boolGauge(burstApplied) {
		t.Fatalf("burst series %#v applied %v", burstSamples, burstApplied)
	}
	assertLabeledGauge(t, "interactive_node_controller_eviction_outcome", node.Name, "outcome", outcomeForReason(evictionReason), 1)
	if evictionReason != EvictionReasonBlocked && len(samplesForNode(t, "interactive_node_controller_eviction_blocked", node.Name)) != 0 {
		t.Fatal("blocked series remained after a non-blocked outcome")
	}
}

func outcomeForReason(reason string) string {
	switch reason {
	case EvictionReasonDisabled:
		return evictionStateDisabled
	case EvictionReasonAuditOnly:
		return evictionStateAuditOnly
	case EvictionReasonInactive:
		return evictionStateInactive
	case EvictionReasonNoEligible:
		return evictionStateNoEligible
	case EvictionReasonBlocked:
		return evictionStateBlocked
	case EvictionReasonBudgetWaiting:
		return evictionStateBudgetWaiting
	case EvictionReasonCompleted:
		return evictionStateCompleted
	default:
		return ""
	}
}

func assertGauge(t *testing.T, name, node, label, value string, want float64) {
	t.Helper()
	got := samplesForNode(t, name, node)
	if label == "" {
		if len(got) != 1 || got[0].value != want {
			t.Fatalf("%s = %#v, want %v", name, got, want)
		}
		return
	}
	assertLabeledGauge(t, name, node, label, value, want)
}

func assertLabeledGauge(t *testing.T, name, node, label, value string, want float64) {
	t.Helper()
	for _, sample := range samplesForNode(t, name, node) {
		if sample.labels[label] == value {
			if sample.value != want {
				t.Fatalf("%s{%s=%s} = %v, want %v", name, label, value, sample.value, want)
			}
			return
		}
	}
	t.Fatalf("%s missing %s=%s in %#v", name, label, value, samplesForNode(t, name, node))
}

func assertNodeSeriesCleared(t *testing.T, nodeName string) {
	t.Helper()
	for _, name := range []string{
		"interactive_node_controller_activity",
		"interactive_node_controller_taint",
		"interactive_node_controller_schedulable",
		"interactive_node_controller_admission",
		"interactive_node_controller_heartbeat_age_seconds",
		"interactive_node_controller_heartbeat_valid",
		"interactive_node_controller_burst_taint",
		"interactive_node_controller_next_transition_timestamp_seconds",
		"interactive_node_controller_eviction_outcome",
		"interactive_node_controller_eviction_blocked",
		"interactive_node_controller_config_invalid",
		"inc_window_active",
		"inc_next_window_start_timestamp_seconds",
	} {
		if got := samplesForNode(t, name, nodeName); len(got) != 0 {
			t.Fatalf("%s still published for %s: %#v", name, nodeName, got)
		}
	}
}

type metricSample struct {
	labels map[string]string
	value  float64
}

func samplesForNode(t *testing.T, name, nodeName string) []metricSample {
	t.Helper()
	var got []metricSample
	for _, sample := range metricSamples(t, name) {
		if sample.labels["node"] == nodeName {
			got = append(got, sample)
		}
	}
	return got
}

func metricSamples(t *testing.T, name string) []metricSample {
	t.Helper()
	families, err := crmetrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var got []metricSample
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			got = append(got, metricSample{labels: labels, value: metric.GetGauge().GetValue()})
		}
	}
	return got
}

func metricHelp(t *testing.T, name string) string {
	t.Helper()
	families, err := crmetrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == name {
			return family.GetHelp()
		}
	}
	t.Fatalf("metric %s is not registered", name)
	return ""
}
