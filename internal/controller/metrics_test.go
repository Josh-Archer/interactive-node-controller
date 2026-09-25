package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestMetricsObserveAndTransitions(t *testing.T) {
	resetMetricsForTesting()
	defer resetMetricsForTesting()

	nodeName := "worker-metrics-1"
	key := "availability.interactive-node.io/state"

	// 1. Initial observation: active game
	gameTaint := &corev1.Taint{Key: key, Value: "active", Effect: corev1.TaintEffectNoSchedule}
	observeActivityAndTaint(nodeName, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, gameTaint)

	if got := testutil.ToFloat64(activityMetric.WithLabelValues(nodeName, "active", "game")); got != 1 {
		t.Fatalf("expected activityMetric to be 1, got %v", got)
	}
	if got := testutil.ToFloat64(taintMetric.WithLabelValues(nodeName, key, "active", string(corev1.TaintEffectNoSchedule))); got != 1 {
		t.Fatalf("expected taintMetric to be 1, got %v", got)
	}
	if count := testutil.CollectAndCount(activityMetric); count != 1 {
		t.Fatalf("expected 1 activity series, got %d", count)
	}
	if count := testutil.CollectAndCount(taintMetric); count != 1 {
		t.Fatalf("expected 1 taint series, got %d", count)
	}

	// 2. Transition to interactive desktop
	interactiveTaint := &corev1.Taint{Key: key, Value: "interactive", Effect: corev1.TaintEffectPreferNoSchedule}
	observeActivityAndTaint(nodeName, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityInteractive, interactiveTaint)

	if got := testutil.ToFloat64(activityMetric.WithLabelValues(nodeName, "active", "interactive")); got != 1 {
		t.Fatalf("expected interactive activityMetric to be 1, got %v", got)
	}
	if got := testutil.ToFloat64(taintMetric.WithLabelValues(nodeName, key, "interactive", string(corev1.TaintEffectPreferNoSchedule))); got != 1 {
		t.Fatalf("expected interactive taintMetric to be 1, got %v", got)
	}
	// Old series should have been deleted
	if count := testutil.CollectAndCount(activityMetric); count != 1 {
		t.Fatalf("expected exactly 1 activity series after transition, got %d", count)
	}
	if count := testutil.CollectAndCount(taintMetric); count != 1 {
		t.Fatalf("expected exactly 1 taint series after transition, got %d", count)
	}

	// 3. Transition to idle (desired taint is nil)
	observeActivityAndTaint(nodeName, availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, nil)

	if got := testutil.ToFloat64(activityMetric.WithLabelValues(nodeName, "idle", "idle")); got != 1 {
		t.Fatalf("expected idle activityMetric to be 1, got %v", got)
	}
	if count := testutil.CollectAndCount(activityMetric); count != 1 {
		t.Fatalf("expected exactly 1 activity series after idle transition, got %d", count)
	}
	// Taint series must be deleted when desired taint is nil
	if count := testutil.CollectAndCount(taintMetric); count != 0 {
		t.Fatalf("expected 0 taint series when idle, got %d", count)
	}

	// 4. Repeated idle observe is idempotent
	observeActivityAndTaint(nodeName, availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, nil)
	if count := testutil.CollectAndCount(activityMetric); count != 1 {
		t.Fatalf("expected exactly 1 activity series on repeated idle, got %d", count)
	}
	if count := testutil.CollectAndCount(taintMetric); count != 0 {
		t.Fatalf("expected 0 taint series on repeated idle, got %d", count)
	}

	// 5. Fail-closed unavailable taint
	failClosedTaint := &corev1.Taint{Key: key, Value: "unavailable", Effect: corev1.TaintEffectNoSchedule}
	observeActivityAndTaint(nodeName, availabilityv1alpha1.StateStale, availabilityv1alpha1.ActivityUnknown, failClosedTaint)

	if got := testutil.ToFloat64(activityMetric.WithLabelValues(nodeName, "stale", "unknown")); got != 1 {
		t.Fatalf("expected stale activityMetric to be 1, got %v", got)
	}
	if got := testutil.ToFloat64(taintMetric.WithLabelValues(nodeName, key, "unavailable", string(corev1.TaintEffectNoSchedule))); got != 1 {
		t.Fatalf("expected unavailable taintMetric to be 1, got %v", got)
	}
	if count := testutil.CollectAndCount(activityMetric); count != 1 {
		t.Fatalf("expected 1 activity series, got %d", count)
	}
	if count := testutil.CollectAndCount(taintMetric); count != 1 {
		t.Fatalf("expected 1 taint series, got %d", count)
	}

	// 6. Clear metrics for node
	clearNodeMetrics(nodeName)
	if count := testutil.CollectAndCount(activityMetric); count != 0 {
		t.Fatalf("expected 0 activity series after clearNodeMetrics, got %d", count)
	}
	if count := testutil.CollectAndCount(taintMetric); count != 0 {
		t.Fatalf("expected 0 taint series after clearNodeMetrics, got %d", count)
	}
}

func TestMetricsEdgeCases(t *testing.T) {
	resetMetricsForTesting()
	defer resetMetricsForTesting()

	// Empty nodeName should be ignored
	observeActivityAndTaint("", availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, nil)
	observeActivityAndTaint("   ", availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, nil)
	clearNodeMetrics("")
	clearNodeMetrics("   ")
	if count := testutil.CollectAndCount(activityMetric); count != 0 {
		t.Fatalf("expected 0 activity series for empty nodeName, got %d", count)
	}
	if count := testutil.CollectAndCount(taintMetric); count != 0 {
		t.Fatalf("expected 0 taint series for empty nodeName, got %d", count)
	}

	// Empty state and activity fallback to unknown
	observeActivityAndTaint("worker-edge", "", "", nil)
	if got := testutil.ToFloat64(activityMetric.WithLabelValues("worker-edge", "unknown", "unknown")); got != 1 {
		t.Fatalf("expected fallback to unknown, got %v", got)
	}
	if count := testutil.CollectAndCount(activityMetric); count != 1 {
		t.Fatalf("expected 1 activity series, got %d", count)
	}
	clearNodeMetrics("worker-edge")
	if count := testutil.CollectAndCount(activityMetric); count != 0 {
		t.Fatalf("expected 0 activity series after clear, got %d", count)
	}
}

func TestMetricsConcurrency(t *testing.T) {
	resetMetricsForTesting()
	defer resetMetricsForTesting()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			node := "node-concurrent"
			for j := 0; j < 50; j++ {
				taint := &corev1.Taint{Key: "key", Value: "val", Effect: corev1.TaintEffectNoSchedule}
				observeActivityAndTaint(node, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, taint)
				observeActivityAndTaint(node, availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, nil)
				clearNodeMetrics(node)
			}
		}(i)
	}
	wg.Wait()
}

func TestReconcileExportsMetricsAndCleansUp(t *testing.T) {
	resetMetricsForTesting()
	defer resetMetricsForTesting()

	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	nodeName := "workstation-metrics"
	activity := &availabilityv1alpha1.NodeActivity{
		ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "availability"},
		Spec:       availabilityv1alpha1.NodeActivitySpec{NodeName: nodeName},
		Status: availabilityv1alpha1.NodeActivityStatus{
			State: availabilityv1alpha1.StateActive, Activity: availabilityv1alpha1.ActivityGame, HeartbeatAt: now,
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
	}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = availabilityv1alpha1.AddToScheme(scheme)

	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(activity).WithObjects(activity, node).Build()
	reconciler := &NodeActivityReconciler{Client: client, Clock: clocktesting.NewFakeClock(now), Policy: testPolicy()}

	// 1. Initial reconcile: should export active game metrics
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}}); err != nil {
		t.Fatal(err)
	}

	if got := testutil.ToFloat64(activityMetric.WithLabelValues(nodeName, "active", "game")); got != 1 {
		t.Fatalf("expected activityMetric to be 1, got %v", got)
	}
	if got := testutil.ToFloat64(taintMetric.WithLabelValues(nodeName, testPolicy().Key, "active", string(corev1.TaintEffectNoSchedule))); got != 1 {
		t.Fatalf("expected taintMetric to be 1, got %v", got)
	}

	// 2. Update to idle: should update activity and remove taint metric
	if err := client.Get(context.Background(), types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}, activity); err != nil {
		t.Fatal(err)
	}
	activity.Status.State = availabilityv1alpha1.StateIdle
	activity.Status.Activity = availabilityv1alpha1.ActivityIdle
	if err := client.Status().Update(context.Background(), activity); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}}); err != nil {
		t.Fatal(err)
	}

	if got := testutil.ToFloat64(activityMetric.WithLabelValues(nodeName, "idle", "idle")); got != 1 {
		t.Fatalf("expected activityMetric to be 1 for idle, got %v", got)
	}
	if count := testutil.CollectAndCount(activityMetric); count != 1 {
		t.Fatalf("expected 1 activity series, got %d", count)
	}
	if count := testutil.CollectAndCount(taintMetric); count != 0 {
		t.Fatalf("expected 0 taint series after idle reconcile, got %d", count)
	}

	// 3. Delete NodeActivity: should clean up metrics
	if err := client.Get(context.Background(), types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}, activity); err != nil {
		t.Fatal(err)
	}
	if err := client.Delete(context.Background(), activity); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}}); err != nil {
		t.Fatal(err)
	}

	if count := testutil.CollectAndCount(activityMetric); count != 0 {
		t.Fatalf("expected 0 activity series after delete, got %d", count)
	}
	if count := testutil.CollectAndCount(taintMetric); count != 0 {
		t.Fatalf("expected 0 taint series after delete, got %d", count)
	}
}

func TestReconcileNodeNotFoundClearsMetrics(t *testing.T) {
	resetMetricsForTesting()
	defer resetMetricsForTesting()

	nodeName := "missing-node"
	// Pre-populate some metrics for this node
	observeActivityAndTaint(nodeName, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, &corev1.Taint{Key: "key", Value: "val", Effect: corev1.TaintEffectNoSchedule})
	if count := testutil.CollectAndCount(activityMetric); count != 1 {
		t.Fatalf("expected 1 activity series, got %d", count)
	}

	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	activity := &availabilityv1alpha1.NodeActivity{
		ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "availability"},
		Spec:       availabilityv1alpha1.NodeActivitySpec{NodeName: nodeName},
		Status: availabilityv1alpha1.NodeActivityStatus{
			State: availabilityv1alpha1.StateActive, Activity: availabilityv1alpha1.ActivityGame, HeartbeatAt: now,
		},
	}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = availabilityv1alpha1.AddToScheme(scheme)

	// Node is intentionally NOT added to objects
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(activity).WithObjects(activity).Build()
	reconciler := &NodeActivityReconciler{Client: client, Clock: clocktesting.NewFakeClock(now), Policy: testPolicy()}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}}); err != nil {
		t.Fatal(err)
	}

	if count := testutil.CollectAndCount(activityMetric); count != 0 {
		t.Fatalf("expected metrics to be cleared when node not found, got %d", count)
	}
	if count := testutil.CollectAndCount(taintMetric); count != 0 {
		t.Fatalf("expected taint metrics to be cleared when node not found, got %d", count)
	}
}
