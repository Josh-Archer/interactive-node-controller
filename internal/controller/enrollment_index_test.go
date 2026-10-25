package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func indexNodeActivities(builder *fake.ClientBuilder) *fake.ClientBuilder {
	return builder.WithIndex(&availabilityv1alpha1.NodeActivity{}, NodeActivityNodeNameKey, IndexNodeActivityNodeName)
}

func TestIndexNodeActivityNodeName(t *testing.T) {
	activity := &availabilityv1alpha1.NodeActivity{Spec: availabilityv1alpha1.NodeActivitySpec{NodeName: "desktop"}}
	values := IndexNodeActivityNodeName(activity)
	if len(values) != 1 || values[0] != "desktop" {
		t.Fatalf("expected [desktop], got %#v", values)
	}
	if values := IndexNodeActivityNodeName(&availabilityv1alpha1.NodeActivity{}); values != nil {
		t.Fatalf("expected nil for empty nodeName, got %#v", values)
	}
	spaced := &availabilityv1alpha1.NodeActivity{Spec: availabilityv1alpha1.NodeActivitySpec{NodeName: "desktop "}}
	if values := IndexNodeActivityNodeName(spaced); len(values) != 1 || values[0] != "desktop " {
		t.Fatalf("expected raw nodeName, got %#v", values)
	}
	if values := IndexNodeActivityNodeName(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "desktop"}}); values != nil {
		t.Fatalf("expected nil for a non-NodeActivity, got %#v", values)
	}
}

func TestEnrollmentIndexCreationDeletionDuplicatesAndDrift(t *testing.T) {
	resetMetricsForTesting()
	defer resetMetricsForTesting()
	ctx := context.Background()
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	desktop := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "desktop"}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: "unrelated", Value: "keep", Effect: corev1.TaintEffectNoSchedule}}}}
	laptopNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "laptop", Labels: map[string]string{"unrelated": "keep"}}}
	game := enrolledActivity("availability", "game", "desktop", availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, now)
	idle := enrolledActivity("other", "idle", "desktop", availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, now)
	laptop := enrolledActivity("other", "laptop", "laptop", availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, now)
	spaced := enrolledActivity("other", "spaced", "desktop ", availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, now)
	objects := []client.Object{desktop, laptopNode, game, idle, laptop, spaced}
	const offNode = 30
	for i := 0; i < offNode; i++ {
		name := fmt.Sprintf("node-%02d", i)
		objects = append(objects,
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}},
			enrolledActivity("batch", name, name, availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, now),
		)
	}
	observed := &observingClient{Client: indexedRuntimeClient(t, objects...)}
	reconciler := &NodeActivityReconciler{Client: observed, Clock: clocktesting.NewFakeClock(now), Policy: testPolicy()}

	protection, err := reconciler.protectionForNode(ctx, "desktop", now)
	if err != nil {
		t.Fatal(err)
	}
	observed.expectLast(t, "desktop", 2)
	if protection.count != 2 || protection.taint == nil || protection.taint.Value != testPolicy().ActiveValue || protection.taint.Effect != corev1.TaintEffectNoSchedule {
		t.Fatalf("protection = %#v taint=%#v", protection, protection.taint)
	}
	if protection.source == nil || protection.source.Name != game.Name || protection.source.Namespace != game.Namespace {
		t.Fatalf("source = %#v", protection.source)
	}
	protection, err = reconciler.protectionForNode(ctx, "laptop", now)
	if err != nil {
		t.Fatal(err)
	}
	observed.expectLast(t, "laptop", 1)
	if protection.count != 1 || protection.taint != nil {
		t.Fatalf("laptop protection = %#v", protection)
	}
	protection, err = reconciler.protectionForNode(ctx, "desktop ", now)
	if err != nil {
		t.Fatal(err)
	}
	observed.expectLast(t, "desktop ", 1)
	if protection.count != 1 || protection.source == nil || protection.source.Name != spaced.Name {
		t.Fatalf("spaced protection = %#v", protection)
	}
	before := len(observed.lists)
	if requests := reconciler.requestsForEnrolledNode(ctx, &corev1.Node{}); requests != nil || len(observed.lists) != before {
		t.Fatalf("empty node name listed enrollments: %#v", requests)
	}
	if requests := reconciler.requestsForEnrolledNode(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "desktop"}}); requests != nil {
		t.Fatalf("non-node requests = %#v", requests)
	}
	assertRequestKeys(t, reconciler.requestsForNode(ctx, game), "availability/game", "other/idle")
	observed.expectLast(t, "desktop", 2)
	assertRequestKeys(t, reconciler.requestsForEnrolledNode(ctx, desktop), "availability/game", "other/idle")
	observed.expectLast(t, "desktop", 2)
	if requests := reconciler.requestsForEnrolledNode(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "absent"}}); len(requests) != 0 {
		t.Fatalf("absent node requests = %#v", requests)
	}
	observed.expectLast(t, "absent", 0)

	extra := enrolledActivity("created", "extra", "desktop", availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, now)
	if err := reconciler.Create(ctx, extra); err != nil {
		t.Fatal(err)
	}
	extra.Status = availabilityv1alpha1.NodeActivityStatus{State: availabilityv1alpha1.StateIdle, Activity: availabilityv1alpha1.ActivityIdle, HeartbeatAt: now}
	if err := reconciler.Status().Update(ctx, extra); err != nil {
		t.Fatal(err)
	}
	protection, err = reconciler.protectionForNode(ctx, "desktop", now)
	if err != nil {
		t.Fatal(err)
	}
	observed.expectLast(t, "desktop", 3)
	if protection.count != 3 {
		t.Fatalf("count after create = %d", protection.count)
	}
	offNodeExtra := enrolledActivity("created", "off-node", "node-off", availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, now)
	if err := reconciler.Create(ctx, offNodeExtra); err != nil {
		t.Fatal(err)
	}
	protection, err = reconciler.protectionForNode(ctx, "desktop", now)
	if err != nil {
		t.Fatal(err)
	}
	observed.expectLast(t, "desktop", 3)
	if protection.count != 3 {
		t.Fatalf("off-node create changed desktop count to %d", protection.count)
	}

	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(extra), extra); err != nil {
		t.Fatal(err)
	}
	controllerutil.AddFinalizer(extra, NodeActivityFinalizer)
	if err := reconciler.Update(ctx, extra); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Delete(ctx, extra); err != nil {
		t.Fatal(err)
	}
	protection, err = reconciler.protectionForNode(ctx, "desktop", now)
	if err != nil {
		t.Fatal(err)
	}
	observed.expectLast(t, "desktop", 3)
	if protection.count != 2 || protection.taint == nil || protection.taint.Value != testPolicy().ActiveValue {
		t.Fatalf("terminating enrollment voted: count=%d taint=%#v", protection.count, protection.taint)
	}
	assertRequestKeys(t, reconciler.requestsForNode(ctx, extra), "availability/game", "other/idle", "created/extra")
	observed.expectLast(t, "desktop", 3)

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: idle.Namespace, Name: idle.Name}}); err != nil {
		t.Fatal(err)
	}
	node := gotNode(t, reconciler.Client, desktop.Name)
	if !hasTaint(node, "unrelated", "keep", corev1.TaintEffectNoSchedule) || !hasTaint(node, testPolicy().Key, testPolicy().ActiveValue, corev1.TaintEffectNoSchedule) {
		t.Fatalf("duplicate reconcile lost active protection: %#v", node.Spec.Taints)
	}
	if reason := conditionReason(t, reconciler.Client, idle.Namespace, idle.Name); reason != "DuplicateEnrollment" {
		t.Fatalf("idle condition = %s", reason)
	}
	laptopSeen := gotNode(t, reconciler.Client, laptopNode.Name)
	if len(laptopSeen.Spec.Taints) != 0 || laptopSeen.Labels["unrelated"] != "keep" || len(laptopSeen.Labels) != 1 {
		t.Fatalf("laptop changed: taints=%#v labels=%#v", laptopSeen.Spec.Taints, laptopSeen.Labels)
	}

	node.Spec.Taints = []corev1.Taint{{Key: "unrelated", Value: "keep", Effect: corev1.TaintEffectNoSchedule}, {Key: testPolicy().Key, Value: "tampered", Effect: corev1.TaintEffectPreferNoSchedule}}
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	node.Labels[SchedulableNodeLabel] = "true"
	if err := reconciler.Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	requests := reconciler.requestsForEnrolledNode(ctx, node)
	assertRequestKeys(t, requests, "availability/game", "other/idle", "created/extra")
	observed.expectLast(t, "desktop", 3)
	for _, request := range requests {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	node = gotNode(t, reconciler.Client, desktop.Name)
	if !hasTaint(node, "unrelated", "keep", corev1.TaintEffectNoSchedule) || !hasTaint(node, testPolicy().Key, testPolicy().ActiveValue, corev1.TaintEffectNoSchedule) {
		t.Fatalf("node drift was not restored: %#v", node.Spec.Taints)
	}
	if _, labeled := node.Labels[SchedulableNodeLabel]; labeled {
		t.Fatalf("schedulable label remained: %#v", node.Labels)
	}
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(extra), &availabilityv1alpha1.NodeActivity{}); !apierrors.IsNotFound(err) {
		t.Fatalf("terminating enrollment after finalizer reconcile: %v", err)
	}
	protection, err = reconciler.protectionForNode(ctx, "desktop", now)
	if err != nil {
		t.Fatal(err)
	}
	observed.expectLast(t, "desktop", 2)
	if protection.count != 2 {
		t.Fatalf("count after finalizer = %d", protection.count)
	}
	offNodeSeen := gotNode(t, reconciler.Client, "node-00")
	if len(offNodeSeen.Spec.Taints) != 0 || len(offNodeSeen.Labels) != 0 {
		t.Fatalf("off-node node changed: %#v %#v", offNodeSeen.Spec.Taints, offNodeSeen.Labels)
	}

	if err := reconciler.Delete(ctx, idle); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: idle.Namespace, Name: idle.Name}}); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(idle), &availabilityv1alpha1.NodeActivity{}); !apierrors.IsNotFound(err) {
		t.Fatalf("deleted duplicate still exists: %v", err)
	}
	assertRequestKeys(t, reconciler.requestsForNode(ctx, idle), "availability/game")
	observed.expectLast(t, "desktop", 1)
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: game.Namespace, Name: game.Name}}); err != nil {
		t.Fatal(err)
	}
	if reason := conditionReason(t, reconciler.Client, game.Namespace, game.Name); reason != "TaintReconciled" {
		t.Fatalf("survivor condition = %s", reason)
	}
	node = gotNode(t, reconciler.Client, desktop.Name)
	if !hasTaint(node, testPolicy().Key, testPolicy().ActiveValue, corev1.TaintEffectNoSchedule) {
		t.Fatalf("survivor lost its taint: %#v", node.Spec.Taints)
	}
	if testutil.CollectAndCount(activityMetric) != 1 || testutil.CollectAndCount(taintMetric) != 1 {
		t.Fatal("deleting the duplicate removed survivor metrics")
	}

	if err := reconciler.Delete(ctx, game); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: game.Namespace, Name: game.Name}}); err != nil {
		t.Fatal(err)
	}
	node = gotNode(t, reconciler.Client, desktop.Name)
	if !hasTaint(node, "unrelated", "keep", corev1.TaintEffectNoSchedule) {
		t.Fatalf("unrelated taint changed at final deletion: %#v", node.Spec.Taints)
	}
	for _, taint := range node.Spec.Taints {
		if taint.Key == testPolicy().Key {
			t.Fatalf("final enrollment left its owned taint: %#v", node.Spec.Taints)
		}
	}
	protection, err = reconciler.protectionForNode(ctx, "desktop", now)
	if err != nil {
		t.Fatal(err)
	}
	observed.expectLast(t, "desktop", 0)
	if protection.count != 0 || protection.taint != nil {
		t.Fatalf("protection after final deletion = %#v", protection)
	}
	if testutil.CollectAndCount(activityMetric) != 0 || testutil.CollectAndCount(taintMetric) != 0 {
		t.Fatal("final deletion left metrics")
	}
	laptopSeen = gotNode(t, reconciler.Client, laptopNode.Name)
	if len(laptopSeen.Spec.Taints) != 0 || laptopSeen.Labels["unrelated"] != "keep" {
		t.Fatalf("laptop changed after desktop deletion: %#v %#v", laptopSeen.Spec.Taints, laptopSeen.Labels)
	}
	protection, err = reconciler.protectionForNode(ctx, "laptop", now)
	if err != nil {
		t.Fatal(err)
	}
	observed.expectLast(t, "laptop", 1)
	if protection.count != 1 {
		t.Fatalf("laptop count = %d", protection.count)
	}
}

func TestEnrollmentIndexDeterministicOrder(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		loser     *availabilityv1alpha1.NodeActivity
		winner    *availabilityv1alpha1.NodeActivity
		wantSpace string
		wantName  string
	}{
		{
			name:      "namespace",
			loser:     enrolledActivity("b", "early", "desktop", availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, now.Add(-2*time.Minute)),
			winner:    enrolledActivity("a", "late", "desktop", availabilityv1alpha1.StateUnknown, availabilityv1alpha1.ActivityUnknown, now),
			wantSpace: "a",
			wantName:  "late",
		},
		{
			name:      "name",
			loser:     enrolledActivity("availability", "z", "desktop", availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, now.Add(-2*time.Minute)),
			winner:    enrolledActivity("availability", "a", "desktop", availabilityv1alpha1.StateUnknown, availabilityv1alpha1.ActivityUnknown, now),
			wantSpace: "availability",
			wantName:  "a",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "desktop"}}
			reconciler := &NodeActivityReconciler{
				Client: indexedRuntimeClient(t, test.loser, test.winner, node),
				Clock:  clocktesting.NewFakeClock(now),
				Policy: testPolicy(),
			}
			protection, err := reconciler.protectionForNode(ctx, "desktop", now)
			if err != nil {
				t.Fatal(err)
			}
			got := "<nil>"
			if protection.source != nil {
				got = protection.source.Namespace + "/" + protection.source.Name
			}
			if protection.count != 2 || got != test.wantSpace+"/"+test.wantName {
				t.Fatalf("source = %s count=%d", got, protection.count)
			}
			if protection.taint == nil || protection.taint.Value != testPolicy().FailClosedValue || protection.taint.Effect != corev1.TaintEffectNoSchedule {
				t.Fatalf("taint = %#v", protection.taint)
			}
		})
	}
}

func TestEnrollmentQueriesFailClosedWithoutIndex(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	activity := enrolledActivity("availability", "game", "desktop", availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, now)
	other := enrolledActivity("other", "laptop", "laptop", availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, now)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "desktop"}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: testPolicy().Key, Value: testPolicy().ActiveValue, Effect: corev1.TaintEffectNoSchedule}}}}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := availabilityv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	unindexed := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&availabilityv1alpha1.NodeActivity{}).WithObjects(activity, other, node).Build()
	reconciler := &NodeActivityReconciler{Client: unindexed, Clock: clocktesting.NewFakeClock(now), Policy: testPolicy()}
	if _, err := reconciler.protectionForNode(ctx, "desktop", now); err == nil || !strings.Contains(err.Error(), "index") {
		t.Fatalf("protection error = %v, want missing index", err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: activity.Namespace, Name: activity.Name}}); err == nil || !strings.Contains(err.Error(), "index") {
		t.Fatalf("reconcile error = %v, want missing index", err)
	}
	seen := gotNode(t, reconciler.Client, node.Name)
	if !hasTaint(seen, testPolicy().Key, testPolicy().ActiveValue, corev1.TaintEffectNoSchedule) || len(seen.Spec.Taints) != 1 {
		t.Fatalf("missing index changed the node: %#v", seen.Spec.Taints)
	}
	if requests := reconciler.requestsForNode(ctx, activity); requests != nil {
		t.Fatalf("sibling requests without index = %#v", requests)
	}
	if requests := reconciler.requestsForEnrolledNode(ctx, node); requests != nil {
		t.Fatalf("node requests without index = %#v", requests)
	}
}

func TestEnrollmentIndexWorkScalesWithMatches(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	const offNode = 200
	const matches = 3
	objects := make([]client.Object, 0, offNode+matches+1)
	objects = append(objects, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "desktop"}})
	var sample *availabilityv1alpha1.NodeActivity
	for i := 0; i < matches; i++ {
		item := enrolledActivity("matches", fmt.Sprintf("desktop-%d", i), "desktop", availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, now)
		if sample == nil {
			sample = item
		}
		objects = append(objects, item)
	}
	for i := 0; i < offNode; i++ {
		name := fmt.Sprintf("node-%d", i)
		objects = append(objects, enrolledActivity("batch", name, name, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, now))
	}
	observed := &observingClient{Client: indexedRuntimeClient(t, objects...)}
	reconciler := &NodeActivityReconciler{Client: observed, Clock: clocktesting.NewFakeClock(now), Policy: testPolicy()}
	protection, err := reconciler.protectionForNode(ctx, "desktop", now)
	if err != nil {
		t.Fatal(err)
	}
	observed.expectLast(t, "desktop", matches)
	if protection.count != matches {
		t.Fatalf("protection examined %d enrollments, want %d matching of %d enrolled", protection.count, matches, matches+offNode)
	}
	requests := reconciler.requestsForEnrolledNode(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "desktop"}})
	observed.expectLast(t, "desktop", matches)
	if len(requests) != matches {
		t.Fatalf("node drift mapped %d enrollments, want %d", len(requests), matches)
	}
	requests = reconciler.requestsForNode(ctx, sample)
	observed.expectLast(t, "desktop", matches)
	if len(requests) != matches {
		t.Fatalf("sibling refresh mapped %d enrollments, want %d", len(requests), matches)
	}
}

func TestIndexedProtectionMeasurement(t *testing.T) {
	for _, test := range []struct {
		matches int
		offNode int
	}{
		{matches: 1, offNode: 500},
		{matches: 40, offNode: 500},
	} {
		t.Run(fmt.Sprintf("matches-%d-offnode-%d", test.matches, test.offNode), func(t *testing.T) {
			listed, count, err := measureIndexedProtection(test.matches, test.offNode)
			if err != nil {
				t.Fatal(err)
			}
			if listed != test.matches || count != test.matches {
				t.Fatalf("listed %d and aggregated %d, want %d matching enrollments out of %d", listed, count, test.matches, test.matches+test.offNode)
			}
		})
	}
}

func BenchmarkProtectionForNode_IndexedEnrollments(b *testing.B) {
	// Same number of enrolled nodes. ByIndex returns only spec.nodeName matches,
	// so the one-match case stays far cheaper than evaluating every enrollment.
	b.Run("one-match-among-1000", func(b *testing.B) {
		benchmarkIndexedProtection(b, 1, 999)
	})
	b.Run("one-thousand-matches", func(b *testing.B) {
		benchmarkIndexedProtection(b, 1000, 0)
	})
}

func enrolledActivity(namespace, name, nodeName string, state availabilityv1alpha1.State, activity availabilityv1alpha1.Activity, heartbeat time.Time) *availabilityv1alpha1.NodeActivity {
	return &availabilityv1alpha1.NodeActivity{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       availabilityv1alpha1.NodeActivitySpec{NodeName: nodeName},
		Status:     availabilityv1alpha1.NodeActivityStatus{State: state, Activity: activity, HeartbeatAt: heartbeat},
	}
}

func indexedRuntimeClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := availabilityv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return indexNodeActivities(fake.NewClientBuilder().WithScheme(scheme)).WithStatusSubresource(&availabilityv1alpha1.NodeActivity{}).WithObjects(objects...).Build()
}

func gotNode(t *testing.T, reader client.Reader, name string) *corev1.Node {
	t.Helper()
	node := &corev1.Node{}
	if err := reader.Get(context.Background(), types.NamespacedName{Name: name}, node); err != nil {
		t.Fatal(err)
	}
	return node
}

func conditionReason(t *testing.T, reader client.Reader, namespace, name string) string {
	t.Helper()
	item := &availabilityv1alpha1.NodeActivity{}
	if err := reader.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, item); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(item.Status.Conditions, TaintAppliedCondition)
	if condition == nil {
		t.Fatalf("%s/%s has no %s condition", namespace, name, TaintAppliedCondition)
	}
	return condition.Reason
}

func assertRequestKeys(t *testing.T, requests []ctrl.Request, want ...string) {
	t.Helper()
	got := requestKeys(requests)
	if len(got) != len(want) {
		t.Fatalf("requests = %#v, want %v", got, want)
	}
	for _, key := range want {
		if !got[key] {
			t.Fatalf("requests = %#v, missing %s", got, key)
		}
	}
}

type observedList struct {
	selector string
	count    int
}

type observingClient struct {
	client.Client
	lists []observedList
}

func (c *observingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	activities, ok := list.(*availabilityv1alpha1.NodeActivityList)
	if !ok {
		return nil
	}
	options := &client.ListOptions{}
	options.ApplyOptions(opts)
	selector := ""
	if options.FieldSelector != nil {
		selector = options.FieldSelector.String()
	}
	c.lists = append(c.lists, observedList{selector: selector, count: len(activities.Items)})
	return nil
}

func (c *observingClient) expectLast(t *testing.T, nodeName string, count int) {
	t.Helper()
	if len(c.lists) == 0 {
		t.Fatal("expected an indexed NodeActivity list")
	}
	got := c.lists[len(c.lists)-1]
	want := NodeActivityNodeNameKey + "=" + nodeName
	if got.selector != want || got.count != count {
		t.Fatalf("indexed list selector=%q count=%d, want selector=%q count=%d", got.selector, got.count, want, count)
	}
}

type indexedActivityClient struct {
	client.Client
	indexer cache.Indexer
	listed  int
}

func (c *indexedActivityClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	activities, ok := list.(*availabilityv1alpha1.NodeActivityList)
	if !ok {
		return c.Client.List(ctx, list, opts...)
	}
	options := &client.ListOptions{}
	options.ApplyOptions(opts)
	if options.FieldSelector == nil {
		return fmt.Errorf("NodeActivity list requires the %s index", NodeActivityNodeNameKey)
	}
	requirements := options.FieldSelector.Requirements()
	if len(requirements) != 1 || requirements[0].Field != NodeActivityNodeNameKey {
		return fmt.Errorf("unexpected field selector %q", options.FieldSelector)
	}
	matched, err := c.indexer.ByIndex(NodeActivityNodeNameKey, requirements[0].Value)
	if err != nil {
		return err
	}
	activities.Items = make([]availabilityv1alpha1.NodeActivity, 0, len(matched))
	for _, item := range matched {
		activity, ok := item.(*availabilityv1alpha1.NodeActivity)
		if !ok {
			return fmt.Errorf("indexed object has type %T", item)
		}
		activities.Items = append(activities.Items, *activity)
	}
	c.listed = len(activities.Items)
	return nil
}

func measureIndexedProtection(matches, offNode int) (listed int, count int, err error) {
	reconciler, indexed, now, err := newIndexedProtection(matches, offNode)
	if err != nil {
		return 0, 0, err
	}
	protection, err := reconciler.protectionForNode(context.Background(), "desktop", now)
	if err != nil {
		return indexed.listed, 0, err
	}
	return indexed.listed, protection.count, nil
}

func benchmarkIndexedProtection(b *testing.B, matches, offNode int) {
	b.Helper()
	reconciler, indexed, now, err := newIndexedProtection(matches, offNode)
	if err != nil {
		b.Fatal(err)
	}
	protection, err := reconciler.protectionForNode(context.Background(), "desktop", now)
	if err != nil {
		b.Fatal(err)
	}
	if indexed.listed != matches || protection.count != matches {
		b.Fatalf("listed %d count %d, want %d matches out of %d enrolled", indexed.listed, protection.count, matches, matches+offNode)
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := reconciler.protectionForNode(ctx, "desktop", now); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(matches), "matches/op")
	b.ReportMetric(float64(matches+offNode), "enrolled/op")
}

func newIndexedProtection(matches, offNode int) (*NodeActivityReconciler, *indexedActivityClient, time.Time, error) {
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		NodeActivityNodeNameKey: func(obj interface{}) ([]string, error) {
			activity, ok := obj.(*availabilityv1alpha1.NodeActivity)
			if !ok || activity.Spec.NodeName == "" {
				return nil, nil
			}
			return []string{activity.Spec.NodeName}, nil
		},
	})
	for i := 0; i < matches; i++ {
		if err := indexer.Add(benchmarkActivity(fmt.Sprintf("match-%d", i), "desktop", now)); err != nil {
			return nil, nil, time.Time{}, err
		}
	}
	for i := 0; i < offNode; i++ {
		if err := indexer.Add(benchmarkActivity(fmt.Sprintf("off-%d", i), fmt.Sprintf("node-%d", i), now)); err != nil {
			return nil, nil, time.Time{}, err
		}
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, nil, time.Time{}, err
	}
	if err := availabilityv1alpha1.AddToScheme(scheme); err != nil {
		return nil, nil, time.Time{}, err
	}
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	indexed := &indexedActivityClient{Client: base, indexer: indexer}
	reconciler := &NodeActivityReconciler{Client: indexed, Clock: clocktesting.NewFakeClock(now), Policy: testPolicy()}
	return reconciler, indexed, now, nil
}

func benchmarkActivity(name, nodeName string, now time.Time) *availabilityv1alpha1.NodeActivity {
	activity := enrolledActivity("bench", name, nodeName, availabilityv1alpha1.StateIdle, availabilityv1alpha1.ActivityIdle, now)
	activity.Spec.LeadTime = "15m"
	activity.Spec.Windows = []availabilityv1alpha1.AvailabilityWindow{{
		Schedule: "0 20 * * *",
		Duration: "2h",
		TimeZone: "UTC",
		State:    "game",
	}}
	return activity
}
