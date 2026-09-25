package controller

import (
	"context"
	"testing"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestReconcileOwnsOnlyConfiguredTaint(t *testing.T) {
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	activity := &availabilityv1alpha1.NodeActivity{
		ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "availability"},
		Spec:       availabilityv1alpha1.NodeActivitySpec{NodeName: "workstation-1"},
		Status: availabilityv1alpha1.NodeActivityStatus{
			State: availabilityv1alpha1.StateActive, Activity: availabilityv1alpha1.ActivityGame, HeartbeatAt: now,
		},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "workstation-1"}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: "unrelated", Value: "keep", Effect: corev1.TaintEffectNoSchedule}, {Key: "availability.interactive-node.io/state", Value: "old", Effect: corev1.TaintEffectPreferNoSchedule}}}}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := availabilityv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(activity).WithObjects(activity, node).Build()
	reconciler := &NodeActivityReconciler{Client: client, Clock: clocktesting.NewFakeClock(now), Policy: testPolicy()}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}}); err != nil {
		t.Fatal(err)
	}
	updated := &corev1.Node{}
	if err := client.Get(context.Background(), types.NamespacedName{Name: node.Name}, updated); err != nil {
		t.Fatal(err)
	}
	if len(updated.Spec.Taints) != 2 {
		t.Fatalf("taints = %#v", updated.Spec.Taints)
	}
	if updated.Spec.Taints[0].Key != "unrelated" {
		t.Fatalf("unrelated taint was changed: %#v", updated.Spec.Taints)
	}
	got := updated.Spec.Taints[1]
	if got.Key != testPolicy().Key || got.Value != "active" || got.Effect != corev1.TaintEffectNoSchedule {
		t.Fatalf("managed taint = %#v", got)
	}
	status := &availabilityv1alpha1.NodeActivity{}
	if err := client.Get(context.Background(), types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}, status); err != nil {
		t.Fatal(err)
	}
	if status.Status.ManagedTaint == nil || status.Status.ManagedTaint.Effect != string(corev1.TaintEffectNoSchedule) {
		t.Fatalf("managed taint status = %#v", status.Status.ManagedTaint)
	}
}

func TestReconcileIdleRemovesOnlyOwnedTaint(t *testing.T) {
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	activity := &availabilityv1alpha1.NodeActivity{ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "availability"}, Spec: availabilityv1alpha1.NodeActivitySpec{NodeName: "workstation-1"}, Status: availabilityv1alpha1.NodeActivityStatus{State: availabilityv1alpha1.StateIdle, Activity: availabilityv1alpha1.ActivityIdle, HeartbeatAt: now}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "workstation-1"}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: "unrelated", Effect: corev1.TaintEffectNoSchedule}, {Key: testPolicy().Key, Value: "active", Effect: corev1.TaintEffectNoSchedule}}}}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = availabilityv1alpha1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(activity).WithObjects(activity, node).Build()
	reconciler := &NodeActivityReconciler{Client: client, Clock: clocktesting.NewFakeClock(now), Policy: testPolicy()}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}}); err != nil {
		t.Fatal(err)
	}
	updated := &corev1.Node{}
	_ = client.Get(context.Background(), types.NamespacedName{Name: node.Name}, updated)
	if len(updated.Spec.Taints) != 1 || updated.Spec.Taints[0].Key != "unrelated" {
		t.Fatalf("taints = %#v", updated.Spec.Taints)
	}
}

func TestReconcileStaleHeartbeatFailsClosed(t *testing.T) {
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	activity := &availabilityv1alpha1.NodeActivity{ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "availability"}, Spec: availabilityv1alpha1.NodeActivitySpec{NodeName: "workstation-1"}, Status: availabilityv1alpha1.NodeActivityStatus{State: availabilityv1alpha1.StateIdle, Activity: availabilityv1alpha1.ActivityIdle, HeartbeatAt: now.Add(-2 * time.Minute)}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "workstation-1"}}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = availabilityv1alpha1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(activity).WithObjects(activity, node).Build()
	reconciler := &NodeActivityReconciler{Client: client, Clock: clocktesting.NewFakeClock(now), Policy: testPolicy()}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}}); err != nil {
		t.Fatal(err)
	}
	updated := &corev1.Node{}
	_ = client.Get(context.Background(), types.NamespacedName{Name: node.Name}, updated)
	if len(updated.Spec.Taints) != 1 || updated.Spec.Taints[0].Value != "unavailable" || updated.Spec.Taints[0].Effect != corev1.TaintEffectNoSchedule {
		t.Fatalf("taints = %#v", updated.Spec.Taints)
	}
}

func TestReconcileUnsupportedActiveActivityFailsClosed(t *testing.T) {
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	activity := &availabilityv1alpha1.NodeActivity{ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "availability"}, Spec: availabilityv1alpha1.NodeActivitySpec{NodeName: "workstation-1"}, Status: availabilityv1alpha1.NodeActivityStatus{State: availabilityv1alpha1.StateActive, Activity: availabilityv1alpha1.ActivityUnknown, HeartbeatAt: now}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "workstation-1"}}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = availabilityv1alpha1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(activity).WithObjects(activity, node).Build()
	reconciler := &NodeActivityReconciler{Client: client, Clock: clocktesting.NewFakeClock(now), Policy: testPolicy()}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}}); err != nil {
		t.Fatal(err)
	}
	updated := &corev1.Node{}
	_ = client.Get(context.Background(), types.NamespacedName{Name: node.Name}, updated)
	if len(updated.Spec.Taints) != 1 || updated.Spec.Taints[0].Value != "unavailable" {
		t.Fatalf("taints = %#v", updated.Spec.Taints)
	}
}

func TestReconcileAddsFinalizer(t *testing.T) {
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	activity := &availabilityv1alpha1.NodeActivity{
		ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "availability"},
		Spec:       availabilityv1alpha1.NodeActivitySpec{NodeName: "workstation-1"},
		Status: availabilityv1alpha1.NodeActivityStatus{
			State: availabilityv1alpha1.StateIdle, Activity: availabilityv1alpha1.ActivityIdle, HeartbeatAt: now,
		},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "workstation-1"}}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = availabilityv1alpha1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(activity).WithObjects(activity, node).Build()
	reconciler := &NodeActivityReconciler{Client: client, Clock: clocktesting.NewFakeClock(now), Policy: testPolicy()}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}}); err != nil {
		t.Fatal(err)
	}
	updatedActivity := &availabilityv1alpha1.NodeActivity{}
	if err := client.Get(context.Background(), types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}, updatedActivity); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(updatedActivity, NodeActivityFinalizer) {
		t.Fatalf("expected finalizer %q on activity, got %v", NodeActivityFinalizer, updatedActivity.Finalizers)
	}
}

func TestReconcileDeleteRemovesOwnedTaintAndFinalizer(t *testing.T) {
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	deleteTime := metav1.NewTime(now)
	activity := &availabilityv1alpha1.NodeActivity{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "desktop",
			Namespace:         "availability",
			DeletionTimestamp: &deleteTime,
			Finalizers:        []string{NodeActivityFinalizer},
		},
		Spec: availabilityv1alpha1.NodeActivitySpec{NodeName: "workstation-1"},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "workstation-1"},
		Spec: corev1.NodeSpec{
			Taints: []corev1.Taint{
				{Key: "unrelated", Value: "keep", Effect: corev1.TaintEffectNoSchedule},
				{Key: testPolicy().Key, Value: "active", Effect: corev1.TaintEffectNoSchedule},
			},
		},
	}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = availabilityv1alpha1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(activity).WithObjects(activity, node).Build()
	reconciler := &NodeActivityReconciler{Client: client, Clock: clocktesting.NewFakeClock(now), Policy: testPolicy()}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}}); err != nil {
		t.Fatal(err)
	}
	updatedNode := &corev1.Node{}
	if err := client.Get(context.Background(), types.NamespacedName{Name: node.Name}, updatedNode); err != nil {
		t.Fatal(err)
	}
	if len(updatedNode.Spec.Taints) != 1 || updatedNode.Spec.Taints[0].Key != "unrelated" {
		t.Fatalf("expected only unrelated taint, got %#v", updatedNode.Spec.Taints)
	}
	updatedActivity := &availabilityv1alpha1.NodeActivity{}
	if err := client.Get(context.Background(), types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}, updatedActivity); err == nil {
		if controllerutil.ContainsFinalizer(updatedActivity, NodeActivityFinalizer) {
			t.Fatalf("expected finalizer to be removed, got %v", updatedActivity.Finalizers)
		}
	} else if !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

func TestReconcileDeleteNodeNotFoundRemovesFinalizer(t *testing.T) {
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	deleteTime := metav1.NewTime(now)
	activity := &availabilityv1alpha1.NodeActivity{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "desktop",
			Namespace:         "availability",
			DeletionTimestamp: &deleteTime,
			Finalizers:        []string{NodeActivityFinalizer},
		},
		Spec: availabilityv1alpha1.NodeActivitySpec{NodeName: "workstation-1"},
	}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = availabilityv1alpha1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(activity).WithObjects(activity).Build()
	reconciler := &NodeActivityReconciler{Client: client, Clock: clocktesting.NewFakeClock(now), Policy: testPolicy()}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}}); err != nil {
		t.Fatal(err)
	}
	updatedActivity := &availabilityv1alpha1.NodeActivity{}
	if err := client.Get(context.Background(), types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}, updatedActivity); err == nil {
		if controllerutil.ContainsFinalizer(updatedActivity, NodeActivityFinalizer) {
			t.Fatalf("expected finalizer to be removed even if node is not found, got %v", updatedActivity.Finalizers)
		}
	} else if !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

func TestReconcileDeleteEmptyNodeNameRemovesFinalizer(t *testing.T) {
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	deleteTime := metav1.NewTime(now)
	activity := &availabilityv1alpha1.NodeActivity{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "desktop",
			Namespace:         "availability",
			DeletionTimestamp: &deleteTime,
			Finalizers:        []string{NodeActivityFinalizer},
		},
		Spec: availabilityv1alpha1.NodeActivitySpec{NodeName: ""},
	}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = availabilityv1alpha1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(activity).WithObjects(activity).Build()
	reconciler := &NodeActivityReconciler{Client: client, Clock: clocktesting.NewFakeClock(now), Policy: testPolicy()}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}}); err != nil {
		t.Fatal(err)
	}
	updatedActivity := &availabilityv1alpha1.NodeActivity{}
	if err := client.Get(context.Background(), types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}, updatedActivity); err == nil {
		if controllerutil.ContainsFinalizer(updatedActivity, NodeActivityFinalizer) {
			t.Fatalf("expected finalizer to be removed when nodeName is empty, got %v", updatedActivity.Finalizers)
		}
	} else if !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

func TestReconcileDeleteNodeWithoutOwnedTaintRemovesFinalizer(t *testing.T) {
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	deleteTime := metav1.NewTime(now)
	activity := &availabilityv1alpha1.NodeActivity{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "desktop",
			Namespace:         "availability",
			DeletionTimestamp: &deleteTime,
			Finalizers:        []string{NodeActivityFinalizer},
		},
		Spec: availabilityv1alpha1.NodeActivitySpec{NodeName: "workstation-1"},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "workstation-1"},
		Spec: corev1.NodeSpec{
			Taints: []corev1.Taint{
				{Key: "unrelated", Value: "keep", Effect: corev1.TaintEffectNoSchedule},
			},
		},
	}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = availabilityv1alpha1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(activity).WithObjects(activity, node).Build()
	reconciler := &NodeActivityReconciler{Client: client, Clock: clocktesting.NewFakeClock(now), Policy: testPolicy()}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}}); err != nil {
		t.Fatal(err)
	}
	updatedNode := &corev1.Node{}
	if err := client.Get(context.Background(), types.NamespacedName{Name: node.Name}, updatedNode); err != nil {
		t.Fatal(err)
	}
	if len(updatedNode.Spec.Taints) != 1 || updatedNode.Spec.Taints[0].Key != "unrelated" {
		t.Fatalf("expected unrelated taint preserved, got %#v", updatedNode.Spec.Taints)
	}
	updatedActivity := &availabilityv1alpha1.NodeActivity{}
	if err := client.Get(context.Background(), types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}, updatedActivity); err == nil {
		if controllerutil.ContainsFinalizer(updatedActivity, NodeActivityFinalizer) {
			t.Fatalf("expected finalizer to be removed, got %v", updatedActivity.Finalizers)
		}
	} else if !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

func testPolicy() TaintPolicy {
	return TaintPolicy{Key: "availability.interactive-node.io/state", InteractiveValue: "interactive", ActiveValue: "active", FailClosedValue: "unavailable", StaleAfter: time.Minute, FailClosed: true}
}
