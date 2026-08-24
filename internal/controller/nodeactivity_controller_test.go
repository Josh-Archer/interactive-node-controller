package controller

import (
	"context"
	"testing"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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

func testPolicy() TaintPolicy {
	return TaintPolicy{Key: "availability.interactive-node.io/state", InteractiveValue: "interactive", ActiveValue: "active", FailClosedValue: "unavailable", StaleAfter: time.Minute, FailClosed: true}
}
