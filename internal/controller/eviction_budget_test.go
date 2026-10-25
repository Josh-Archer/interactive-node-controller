package controller

import (
	"context"
	"testing"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestEvictionBudgetDefaultsAreConservative(t *testing.T) {
	policy := EvictionPolicy{}.normalized()
	want := EvictionBudget{PerNode: 1, Global: 1, Interval: time.Minute, BurstPerNode: 1, BurstGlobal: 1}
	if policy.StandardBudget != want || policy.LossyBudget != want {
		t.Fatalf("standard=%#v lossy=%#v, want both %#v", policy.StandardBudget, policy.LossyBudget, want)
	}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}

	custom := EvictionPolicy{
		MaxPerReconcile: 3,
		BudgetInterval:  10 * time.Second,
		LossyBudget:     EvictionBudget{PerNode: 4, BurstPerNode: 6},
	}.normalized()
	if custom.StandardBudget != (EvictionBudget{PerNode: 3, Global: 3, Interval: 10 * time.Second, BurstPerNode: 3, BurstGlobal: 3}) {
		t.Fatalf("standard=%#v", custom.StandardBudget)
	}
	if custom.LossyBudget != (EvictionBudget{PerNode: 4, Global: 4, Interval: 10 * time.Second, BurstPerNode: 6, BurstGlobal: 4}) {
		t.Fatalf("lossy=%#v", custom.LossyBudget)
	}
}

func TestTokenBucketRefusesSameInstantAndRefillsOnTime(t *testing.T) {
	var budgets evictionBudgets
	policy := EvictionPolicy{BudgetInterval: 15 * time.Second}.normalized()
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	if ok, wait := budgets.tryTake(false, "desktop", now, policy); !ok || wait != 0 {
		t.Fatalf("first take ok=%v wait=%s", ok, wait)
	}
	ok, wait := budgets.tryTake(false, "desktop", now, policy)
	if ok || wait != 15*time.Second {
		t.Fatalf("same-instant take ok=%v wait=%s", ok, wait)
	}
	if ok, _ := budgets.tryTake(false, "desktop", now.Add(15*time.Second-time.Nanosecond), policy); ok {
		t.Fatal("refilled before the interval")
	}
	if ok, wait := budgets.tryTake(false, "desktop", now.Add(15*time.Second), policy); !ok {
		t.Fatalf("interval refill wait=%s", wait)
	}
}

func TestTokenBucketBurstThenSteadyRate(t *testing.T) {
	var budgets evictionBudgets
	policy := EvictionPolicy{StandardBudget: EvictionBudget{
		PerNode: 1, Global: 1, Interval: time.Minute, BurstPerNode: 3, BurstGlobal: 3,
	}}.normalized()
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if ok, wait := budgets.tryTake(false, "desktop", now, policy); !ok {
			t.Fatalf("burst take %d wait=%s", i, wait)
		}
	}
	if ok, wait := budgets.tryTake(false, "desktop", now, policy); ok || wait != time.Minute {
		t.Fatalf("burst overflow ok=%v wait=%s", ok, wait)
	}
	if ok, _ := budgets.tryTake(false, "desktop", now.Add(time.Minute), policy); !ok {
		t.Fatal("steady refill did not grant one token")
	}
	if ok, _ := budgets.tryTake(false, "desktop", now.Add(time.Minute), policy); ok {
		t.Fatal("steady refill granted a second token")
	}
}

func TestTokenBucketScopesAreIndependent(t *testing.T) {
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	t.Run("global bucket is shared by nodes", func(t *testing.T) {
		var budgets evictionBudgets
		policy := EvictionPolicy{StandardBudget: EvictionBudget{
			PerNode: 2, Global: 1, Interval: time.Minute, BurstPerNode: 2, BurstGlobal: 1,
		}}.normalized()
		if ok, _ := budgets.tryTake(false, "a", now, policy); !ok {
			t.Fatal("node a")
		}
		if ok, wait := budgets.tryTake(false, "b", now, policy); ok || wait <= 0 {
			t.Fatalf("node b ok=%v wait=%s", ok, wait)
		}
	})
	t.Run("per-node buckets are not shared", func(t *testing.T) {
		var budgets evictionBudgets
		policy := EvictionPolicy{StandardBudget: EvictionBudget{
			PerNode: 1, Global: 5, Interval: time.Minute, BurstPerNode: 1, BurstGlobal: 5,
		}}.normalized()
		if ok, _ := budgets.tryTake(false, "a", now, policy); !ok {
			t.Fatal("node a")
		}
		if ok, _ := budgets.tryTake(false, "b", now, policy); !ok {
			t.Fatal("node b was charged for node a")
		}
		if ok, _ := budgets.tryTake(false, "a", now, policy); ok {
			t.Fatal("node a spent a second per-node token")
		}
	})
	t.Run("standard and lossy buckets are not shared", func(t *testing.T) {
		var budgets evictionBudgets
		policy := EvictionPolicy{BudgetInterval: time.Minute}.normalized()
		if ok, _ := budgets.tryTake(false, "a", now, policy); !ok {
			t.Fatal("standard")
		}
		if ok, _ := budgets.tryTake(true, "a", now, policy); !ok {
			t.Fatal("lossy shared the standard token")
		}
		if ok, _ := budgets.tryTake(false, "a", now, policy); ok {
			t.Fatal("second standard")
		}
		if ok, _ := budgets.tryTake(true, "a", now, policy); ok {
			t.Fatal("second lossy")
		}
	})
}

func TestEvictionFrequentReconcilesCannotBypassBudget(t *testing.T) {
	std1, std2 := eligiblePod("std-1"), eligiblePod("std-2")
	lossy1 := lossyPod("lossy-1")
	lossy2 := lossyPod("lossy-2")
	r, activity, _, base := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{std1, std2, lossy1, lossy2})
	clock := clocktesting.NewFakeClock(activity.Status.HeartbeatAt)
	r.Clock = clock
	r.Eviction.BudgetInterval = 15 * time.Second
	evictor := &removingEvictor{client: r.Client}
	evictor.recordingEvictor = *base
	r.Evictor = evictor

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}}
	var first ctrl.Result
	for i := 0; i < 5; i++ {
		result, err := r.Reconcile(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = result
		}
		if len(evictor.names) != 2 {
			t.Fatalf("reconcile %d evicted %#v", i, evictor.names)
		}
	}
	if first.RequeueAfter != 15*time.Second {
		t.Fatalf("requeue = %s, want budget interval", first.RequeueAfter)
	}
	for _, options := range evictor.options {
		if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil || options.GracePeriodSeconds == nil || *options.GracePeriodSeconds != 20 {
			t.Fatalf("eviction options = %#v", options)
		}
	}
	got := &availabilityv1alpha1.NodeActivity{}
	if err := r.Get(context.Background(), request.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if !got.Status.HeartbeatAt.Equal(activity.Status.HeartbeatAt) {
		t.Fatal("progress required a heartbeat change")
	}
	waiting := metav1.Condition{}
	for _, condition := range got.Status.Conditions {
		if condition.Type == EvictionCondition {
			waiting = condition
		}
	}
	if waiting.Reason != "BudgetWaiting" {
		t.Fatalf("condition after repeated reconcile = %#v", waiting)
	}

	clock.Step(first.RequeueAfter - time.Nanosecond)
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(evictor.names) != 2 {
		t.Fatalf("early refill evicted %#v", evictor.names)
	}
	clock.Step(time.Nanosecond)
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(evictor.names) != 4 {
		t.Fatalf("queued candidates = %#v", evictor.names)
	}
	seen := map[string]int{}
	for _, name := range evictor.names {
		seen[name.Name]++
		if seen[name.Name] != 1 {
			t.Fatalf("duplicate eviction %#v", evictor.names)
		}
	}
}

func TestEvictionBudgetScopesAcrossNodes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	t.Run("global budget limits every node", func(t *testing.T) {
		local := eligiblePod("local")
		remote := eligiblePod("remote")
		remote.Spec.NodeName = "other"
		remote.ResourceVersion = ""
		r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{local})
		if err := r.Create(ctx, remote); err != nil {
			t.Fatal(err)
		}
		clock := clocktesting.NewFakeClock(now)
		r.Clock = clock
		r.Eviction.StandardBudget = EvictionBudget{PerNode: 2, Global: 1, Interval: 15 * time.Second, BurstPerNode: 2, BurstGlobal: 1}
		summary, err := r.reconcileEvictions(ctx, activity, node, &node.Spec.Taints[0])
		if err != nil {
			t.Fatal(err)
		}
		if summary.evicted != 1 {
			t.Fatalf("local summary=%#v", summary)
		}
		otherNode := node.DeepCopy()
		otherNode.Name = "other"
		other := activity.DeepCopy()
		blocked, err := r.reconcileEvictions(ctx, other, otherNode, &otherNode.Spec.Taints[0])
		if err != nil {
			t.Fatal(err)
		}
		if blocked.evicted != 0 || blocked.deferred != 1 || blocked.requeueAfter != 15*time.Second || len(evictor.names) != 1 {
			t.Fatalf("global summary=%#v evictions=%#v", blocked, evictor.names)
		}
		clock.Step(blocked.requeueAfter)
		released, err := r.reconcileEvictions(ctx, other, otherNode, &otherNode.Spec.Taints[0])
		if err != nil {
			t.Fatal(err)
		}
		if released.evicted != 1 || len(evictor.names) != 2 || evictor.names[1].Name != "remote" {
			t.Fatalf("released summary=%#v evictions=%#v", released, evictor.names)
		}
	})
	t.Run("per-node budget does not block another node", func(t *testing.T) {
		local := []*corev1.Pod{eligiblePod("local-1"), eligiblePod("local-2")}
		remote := eligiblePod("remote")
		remote.Spec.NodeName = "other"
		remote.ResourceVersion = ""
		r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, local)
		if err := r.Create(ctx, remote); err != nil {
			t.Fatal(err)
		}
		r.Eviction.StandardBudget = EvictionBudget{PerNode: 1, Global: 5, Interval: time.Minute, BurstPerNode: 1, BurstGlobal: 5}
		if _, err := r.reconcileEvictions(ctx, activity, node, &node.Spec.Taints[0]); err != nil {
			t.Fatal(err)
		}
		otherNode := node.DeepCopy()
		otherNode.Name = "other"
		if _, err := r.reconcileEvictions(ctx, activity, otherNode, &otherNode.Spec.Taints[0]); err != nil {
			t.Fatal(err)
		}
		if len(evictor.names) != 2 {
			t.Fatalf("evictions=%#v", evictor.names)
		}
		names := map[string]bool{}
		for _, name := range evictor.names {
			names[name.Name] = true
		}
		if !names["remote"] || (!names["local-1"] && !names["local-2"]) {
			t.Fatalf("evictions=%#v", evictor.names)
		}
	})
}

func TestEvictionBudgetResetsForNewProcess(t *testing.T) {
	pods := []*corev1.Pod{eligiblePod("one"), eligiblePod("two")}
	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, pods)
	r.Eviction.BudgetInterval = time.Hour
	ctx := context.Background()
	if _, err := r.reconcileEvictions(ctx, activity, node, &node.Spec.Taints[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcileEvictions(ctx, activity, node, &node.Spec.Taints[0]); err != nil {
		t.Fatal(err)
	}
	if len(evictor.names) != 1 {
		t.Fatalf("leader spent more than one token: %#v", evictor.names)
	}
	restarted := &NodeActivityReconciler{Client: r.Client, Clock: r.Clock, Policy: r.Policy, Eviction: r.Eviction, Evictor: evictor}
	if _, err := restarted.reconcileEvictions(ctx, activity, node, &node.Spec.Taints[0]); err != nil {
		t.Fatal(err)
	}
	if len(evictor.names) != 2 {
		t.Fatalf("restart did not grant a new burst: %#v", evictor.names)
	}
	if _, err := r.reconcileEvictions(ctx, activity, node, &node.Spec.Taints[0]); err != nil {
		t.Fatal(err)
	}
	if len(evictor.names) != 2 {
		t.Fatal("previous process regained a token on restart of its successor")
	}
}

func TestEvictionAuditDoesNotSpendBudget(t *testing.T) {
	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{eligiblePod("one"), eligiblePod("two")})
	r.Eviction.Audit = true
	summary, err := r.reconcileEvictions(context.Background(), activity, node, &node.Spec.Taints[0])
	if err != nil {
		t.Fatal(err)
	}
	if summary.audited != 2 || len(evictor.names) != 0 {
		t.Fatalf("audit summary=%#v", summary)
	}
	r.Eviction.Audit = false
	summary, err = r.reconcileEvictions(context.Background(), activity, node, &node.Spec.Taints[0])
	if err != nil {
		t.Fatal(err)
	}
	if summary.evicted != 1 || summary.deferred != 1 {
		t.Fatalf("post-audit summary=%#v evictions=%#v", summary, evictor.names)
	}
}

func TestEvictionBudgetWaitsWhileRetryDeadlineHolds(t *testing.T) {
	blocked := eligiblePod("blocked")
	next := eligiblePod("next")
	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{blocked, next})
	clock := clocktesting.NewFakeClock(activity.Status.HeartbeatAt)
	r.Clock = clock
	r.Eviction.RetryBackoff = 30 * time.Second
	r.Eviction.BudgetInterval = 10 * time.Second
	evictor.err = apierrors.NewTooManyRequests("pdb blocks eviction", 0)
	ctx := context.Background()
	reconcile := func() evictionSummary {
		t.Helper()
		summary, err := r.reconcileEvictions(ctx, activity, node, &node.Spec.Taints[0])
		if err != nil {
			t.Fatal(err)
		}
		return summary
	}
	summary := reconcile()
	if summary.attempted != 1 || summary.blocked != 1 || summary.deferred != 1 || len(evictor.names) != 1 {
		t.Fatalf("first summary=%#v evictions=%#v", summary, evictor.names)
	}
	evictor.err = nil
	summary = reconcile()
	if len(evictor.names) != 1 || summary.deferred != 1 {
		t.Fatalf("budget or deadline bypassed: summary=%#v evictions=%#v", summary, evictor.names)
	}
	first := evictor.names[0].Name
	clock.Step(10 * time.Second)
	summary = reconcile()
	if len(evictor.names) != 2 || evictor.names[1].Name == first || summary.evicted != 1 {
		t.Fatalf("queued pod did not proceed at budget refill: summary=%#v evictions=%#v", summary, evictor.names)
	}
	clock.Step(20 * time.Second)
	summary = reconcile()
	if len(evictor.names) != 3 || evictor.names[2].Name != first {
		t.Fatalf("backoff pod was not retried after its deadline: summary=%#v evictions=%#v", summary, evictor.names)
	}
	if evictor.options[2].Preconditions == nil || evictor.options[2].GracePeriodSeconds == nil || *evictor.options[2].GracePeriodSeconds != 20 {
		t.Fatalf("retry lost preconditions or grace: %#v", evictor.options[2])
	}
}

func lossyPod(name string) *corev1.Pod {
	pod := eligiblePodWithLabels(name, map[string]string{EvictableLabel: "true", AllowLossyEvictionLabel: "true"})
	pod.Spec.Volumes = []corev1.Volume{{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	return pod
}

type removingEvictor struct {
	recordingEvictor
	client client.Client
}

func (e *removingEvictor) Evict(ctx context.Context, namespace, name string, options metav1.DeleteOptions) error {
	if err := e.recordingEvictor.Evict(ctx, namespace, name, options); err != nil {
		return err
	}
	pod := &corev1.Pod{}
	if err := e.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, pod); err != nil {
		return err
	}
	return e.client.Delete(ctx, pod)
}
