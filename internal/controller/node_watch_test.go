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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestRequestsForEnrolledNode(t *testing.T) {
	ctx := context.Background()
	desktop := &availabilityv1alpha1.NodeActivity{ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "availability"}, Spec: availabilityv1alpha1.NodeActivitySpec{NodeName: "desktop"}}
	duplicate := &availabilityv1alpha1.NodeActivity{ObjectMeta: metav1.ObjectMeta{Name: "desktop-dup", Namespace: "other"}, Spec: availabilityv1alpha1.NodeActivitySpec{NodeName: "desktop"}}
	laptop := &availabilityv1alpha1.NodeActivity{ObjectMeta: metav1.ObjectMeta{Name: "laptop", Namespace: "other"}, Spec: availabilityv1alpha1.NodeActivitySpec{NodeName: "laptop"}}
	reconciler := &NodeActivityReconciler{Client: fakeClient(t, desktop, duplicate, laptop), Policy: testPolicy()}

	got := requestKeys(reconciler.requestsForEnrolledNode(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "desktop"}}))
	if len(got) != 2 || !got["availability/desktop"] || !got["other/desktop-dup"] {
		t.Fatalf("desktop requests = %#v", got)
	}
	got = requestKeys(reconciler.requestsForEnrolledNode(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "laptop"}}))
	if len(got) != 1 || !got["other/laptop"] {
		t.Fatalf("laptop requests = %#v", got)
	}
	if requests := reconciler.requestsForEnrolledNode(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "missing"}}); len(requests) != 0 {
		t.Fatalf("missing node requests = %#v", requests)
	}
	if requests := reconciler.requestsForEnrolledNode(ctx, &corev1.Node{}); requests != nil {
		t.Fatalf("empty node name requests = %#v", requests)
	}
	if requests := reconciler.requestsForEnrolledNode(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "desktop"}}); requests != nil {
		t.Fatalf("non-node requests = %#v", requests)
	}
}

func TestOutOfBandManagedTaintReconcilesFromNodeMapping(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	policy := testPolicy()
	policy.BurstTaintKey = "availability.interactive-node.io/burst"
	activity := &availabilityv1alpha1.NodeActivity{
		ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "availability"},
		Spec:       availabilityv1alpha1.NodeActivitySpec{NodeName: "desktop"},
		Status:     availabilityv1alpha1.NodeActivityStatus{State: availabilityv1alpha1.StateActive, Activity: availabilityv1alpha1.ActivityInteractive, HeartbeatAt: now},
	}
	other := &availabilityv1alpha1.NodeActivity{
		ObjectMeta: metav1.ObjectMeta{Name: "laptop", Namespace: "other"},
		Spec:       availabilityv1alpha1.NodeActivitySpec{NodeName: "laptop"},
		Status:     availabilityv1alpha1.NodeActivityStatus{State: availabilityv1alpha1.StateIdle, Activity: availabilityv1alpha1.ActivityIdle, HeartbeatAt: now},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "desktop", Labels: map[string]string{"unrelated": "keep"}}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: "unrelated", Value: "keep", Effect: corev1.TaintEffectNoSchedule}}}}
	laptop := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "laptop"}}
	reconciler := &NodeActivityReconciler{Client: fakeClient(t, activity, other, node, laptop), Clock: clocktesting.NewFakeClock(now), Policy: policy}
	reconcileAll := func(object client.Object) {
		t.Helper()
		for _, request := range reconciler.requestsForEnrolledNode(ctx, object) {
			if _, err := reconciler.Reconcile(ctx, request); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: activity.Namespace, Name: activity.Name}}); err != nil {
		t.Fatal(err)
	}

	fresh := &corev1.Node{}
	if err := reconciler.Get(ctx, types.NamespacedName{Name: node.Name}, fresh); err != nil {
		t.Fatal(err)
	}
	fresh.Spec.Taints = []corev1.Taint{{Key: "unrelated", Value: "keep", Effect: corev1.TaintEffectNoSchedule}, {Key: policy.Key, Value: "tampered", Effect: corev1.TaintEffectNoSchedule}}
	fresh.Labels[SchedulableNodeLabel] = "true"
	if err := reconciler.Update(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	recorded := fresh.DeepCopy()
	reconcileAll(recorded)
	if err := reconciler.Get(ctx, types.NamespacedName{Name: node.Name}, fresh); err != nil {
		t.Fatal(err)
	}
	if !hasTaint(fresh, "unrelated", "keep", corev1.TaintEffectNoSchedule) {
		t.Fatalf("unrelated taint changed: %#v", fresh.Spec.Taints)
	}
	if !hasTaint(fresh, policy.Key, policy.InteractiveValue, corev1.TaintEffectPreferNoSchedule) {
		t.Fatalf("primary taint = %#v", fresh.Spec.Taints)
	}
	if !hasTaint(fresh, policy.BurstTaintKey, policy.FailClosedValue, corev1.TaintEffectNoSchedule) {
		t.Fatalf("burst taint = %#v", fresh.Spec.Taints)
	}
	if fresh.Labels[SchedulableNodeLabel] != "" || fresh.Labels["unrelated"] != "keep" {
		t.Fatalf("labels = %#v", fresh.Labels)
	}
	if err := reconciler.Get(ctx, types.NamespacedName{Name: laptop.Name}, laptop); err != nil {
		t.Fatal(err)
	}
	if len(laptop.Spec.Taints) != 0 || len(laptop.Labels) != 0 {
		t.Fatalf("unrelated node changed: %#v %#v", laptop.Spec.Taints, laptop.Labels)
	}

	// A later out-of-band removal of only the burst taint is restored the same way.
	if err := reconciler.Get(ctx, types.NamespacedName{Name: node.Name}, fresh); err != nil {
		t.Fatal(err)
	}
	kept := make([]corev1.Taint, 0, len(fresh.Spec.Taints))
	for _, taint := range fresh.Spec.Taints {
		if taint.Key != policy.BurstTaintKey {
			kept = append(kept, taint)
		}
	}
	fresh.Spec.Taints = kept
	if err := reconciler.Update(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	reconcileAll(fresh.DeepCopy())
	if err := reconciler.Get(ctx, types.NamespacedName{Name: node.Name}, fresh); err != nil {
		t.Fatal(err)
	}
	if !hasTaint(fresh, policy.BurstTaintKey, policy.FailClosedValue, corev1.TaintEffectNoSchedule) {
		t.Fatalf("burst taint was not restored: %#v", fresh.Spec.Taints)
	}
}

func TestNodeOwnedFieldsChangedPredicate(t *testing.T) {
	policy := testPolicy()
	policy.BurstTaintKey = "availability.interactive-node.io/burst"
	reconciler := &NodeActivityReconciler{Policy: policy}
	predicate := reconciler.nodeOwnedFieldsChanged()
	base := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "desktop", Labels: map[string]string{"unrelated": "keep"}},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{
			{Key: "unrelated", Value: "keep", Effect: corev1.TaintEffectNoSchedule},
			{Key: policy.Key, Value: policy.InteractiveValue, Effect: corev1.TaintEffectPreferNoSchedule},
			{Key: policy.BurstTaintKey, Value: policy.FailClosedValue, Effect: corev1.TaintEffectNoSchedule, TimeAdded: &metav1.Time{Time: time.Unix(10, 0)}},
		}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
	changed := func(mutate func(*corev1.Node)) bool {
		t.Helper()
		next := base.DeepCopy()
		mutate(next)
		return predicate.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: next})
	}
	if changed(func(node *corev1.Node) {
		node.Status.Conditions[0].Status = corev1.ConditionFalse
		node.ResourceVersion = "2"
	}) {
		t.Fatal("status-only update enqueued a reconcile")
	}
	if changed(func(node *corev1.Node) {
		node.Labels["zone"] = "a"
	}) {
		t.Fatal("unrelated label enqueued a reconcile")
	}
	if changed(func(node *corev1.Node) {
		node.Spec.Taints[0].Value = "edited"
	}) {
		t.Fatal("unrelated taint enqueued a reconcile")
	}
	if changed(func(node *corev1.Node) {
		added := metav1.NewTime(time.Unix(99, 0))
		node.Spec.Taints[2].TimeAdded = &added
	}) {
		t.Fatal("TimeAdded-only update enqueued a reconcile")
	}
	if !changed(func(node *corev1.Node) {
		node.Spec.Taints[1].Value = "tampered"
	}) {
		t.Fatal("managed taint value was ignored")
	}
	if !changed(func(node *corev1.Node) {
		node.Spec.Taints[1].Effect = corev1.TaintEffectNoSchedule
	}) {
		t.Fatal("managed taint effect was ignored")
	}
	if !changed(func(node *corev1.Node) {
		node.Spec.Taints = []corev1.Taint{node.Spec.Taints[0], node.Spec.Taints[2]}
	}) {
		t.Fatal("managed taint removal was ignored")
	}
	if !changed(func(node *corev1.Node) {
		node.Spec.Taints[2].Value = "tampered"
	}) {
		t.Fatal("burst taint change was ignored")
	}
	if !changed(func(node *corev1.Node) {
		node.Labels[SchedulableNodeLabel] = "true"
	}) {
		t.Fatal("schedulable label addition was ignored")
	}
	labeled := base.DeepCopy()
	labeled.Labels[SchedulableNodeLabel] = "true"
	cleared := labeled.DeepCopy()
	delete(cleared.Labels, SchedulableNodeLabel)
	if !predicate.Update(event.UpdateEvent{ObjectOld: labeled, ObjectNew: cleared}) {
		t.Fatal("schedulable label removal was ignored")
	}
	if !predicate.Create(event.CreateEvent{Object: base}) {
		t.Fatal("node create was ignored")
	}
	if !predicate.Delete(event.DeleteEvent{Object: base}) {
		t.Fatal("node delete was ignored")
	}
	if predicate.Generic(event.GenericEvent{Object: base}) {
		t.Fatal("generic node event enqueued a reconcile")
	}
	if predicate.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: &corev1.Pod{}}) {
		t.Fatal("non-node update enqueued a reconcile")
	}
}

func fakeClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := availabilityv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&availabilityv1alpha1.NodeActivity{}).WithObjects(objects...).Build()
}

func requestKeys(requests []ctrl.Request) map[string]bool {
	got := make(map[string]bool, len(requests))
	for _, request := range requests {
		got[request.Namespace+"/"+request.Name] = true
	}
	return got
}
