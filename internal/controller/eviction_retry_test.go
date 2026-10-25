package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
)

func TestEvictionRetryDeadlineSurvivesReconcileEvents(t *testing.T) {
	r, activity, _, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{eligiblePod("blocked")})
	clock := clocktesting.NewFakeClock(activity.Status.HeartbeatAt)
	r.Clock = clock
	r.Eviction.RetryBackoff = 17 * time.Second
	// The deadline is the gate under test. A wider burst keeps the time budget
	// from delaying the retry that #35 requires once the deadline is reached.
	r.Eviction.StandardBudget = EvictionBudget{PerNode: 5, Global: 5, Interval: time.Minute, BurstPerNode: 5, BurstGlobal: 5}
	evictor.err = apierrors.NewTooManyRequests("PDB blocks eviction", 0)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}}
	for _, step := range []time.Duration{0, 0, 16 * time.Second} {
		clock.Step(step)
		result, err := r.Reconcile(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if result.RequeueAfter <= 0 {
			t.Fatalf("missing delayed retry: %#v", result)
		}
	}
	if len(evictor.names) != 1 {
		t.Fatalf("early eviction attempts=%d, want 1", len(evictor.names))
	}
	clock.Step(time.Second)
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(evictor.names) != 2 {
		t.Fatalf("attempts at deadline=%d, want 2", len(evictor.names))
	}
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(evictor.names) != 2 {
		t.Fatal("repeated block did not renew deadline")
	}
}

func TestEvictionRetryStateChangesAndReplacement(t *testing.T) {
	ctx := context.Background()
	pod := eligiblePod("blocked")
	pod.UID = types.UID("original")
	r, activity, node, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{pod})
	clock := clocktesting.NewFakeClock(activity.Status.HeartbeatAt)
	r.Clock = clock
	r.Eviction.StandardBudget = EvictionBudget{PerNode: 5, Global: 5, Interval: time.Minute, BurstPerNode: 5, BurstGlobal: 5}
	evictor.err = apierrors.NewTooManyRequests("PDB blocks eviction", 0)
	reconcile := func() evictionSummary {
		t.Helper()
		summary, err := r.reconcileEvictions(ctx, activity, node, &node.Spec.Taints[0])
		if err != nil {
			t.Fatal(err)
		}
		return summary
	}
	reconcile()
	if err := r.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	replacement := eligiblePod("blocked")
	replacement.ResourceVersion = "" // Create assigns a new resource version.
	replacement.UID = types.UID("replacement")
	if err := r.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if len(evictor.names) != 2 {
		t.Fatal("replacement inherited the previous UID's deadline")
	}
	activity.Status.State = availabilityv1alpha1.StateIdle
	reconcile()
	if len(evictor.names) != 2 {
		t.Fatal("idle state evicted")
	}
	activity.Status.State = availabilityv1alpha1.StateActive
	evictor.err = nil
	reconcile()
	if len(evictor.names) != 3 {
		t.Fatal("inactive state did not clear retry state")
	}
	// Removing opt-in must be respected even after a previous failure.
	if err := r.Get(ctx, types.NamespacedName{Name: replacement.Name, Namespace: replacement.Namespace}, replacement); err != nil {
		t.Fatal(err)
	}
	replacement.Labels = nil
	if err := r.Update(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	clock.Step(time.Minute)
	activity.Status.HeartbeatAt = clock.Now()
	reconcile()
	if len(evictor.names) != 3 {
		t.Fatal("non-opted-in Pod evicted")
	}
}

func TestEvictionRetryJitterIsBoundedAndStable(t *testing.T) {
	backoff := 17 * time.Second
	jitter := 5 * time.Second
	policy := EvictionPolicy{RetryBackoff: backoff, RetryJitter: jitter}.normalized()
	shortHint := apierrors.NewTooManyRequests("pdb", 5)
	shortDelay := blockedRetryDelay(retryKey("desktop", eligiblePod("one")), policy, shortHint)
	if shortDelay < backoff || shortDelay > backoff+jitter {
		t.Fatalf("short hint delay=%s, want [%s, %s]", shortDelay, backoff, backoff+jitter)
	}
	longHint := apierrors.NewTooManyRequests("pdb", 45)
	key := retryKey("desktop", eligiblePod("one"))
	longDelay := blockedRetryDelay(key, policy, longHint)
	if longDelay < 45*time.Second || longDelay > 45*time.Second+jitter {
		t.Fatalf("long hint delay=%s", longDelay)
	}
	if blockedRetryDelay(key, policy, longHint) != longDelay {
		t.Fatal("jitter changed for the same Pod UID")
	}
	nonzero := false
	for i := 0; i < 32; i++ {
		pod := eligiblePod(fmt.Sprintf("pod-%d", i))
		got := retryJitter(retryKey("desktop", pod), jitter)
		if got < 0 || got > jitter {
			t.Fatalf("jitter %s outside bound", got)
		}
		if retryJitter(retryKey("desktop", pod), jitter) != got {
			t.Fatal("unstable jitter")
		}
		if got != 0 {
			nonzero = true
		}
	}
	if !nonzero {
		t.Fatal("jitter was always zero")
	}
	if retryJitter(key, 0) != 0 {
		t.Fatal("zero jitter bound applied a delay")
	}
}

func TestEvictionRetryHonorsAPIHintAcrossReconciles(t *testing.T) {
	pod := eligiblePod("blocked")
	r, activity, _, evictor := evictionFixture(t, availabilityv1alpha1.StateActive, availabilityv1alpha1.ActivityGame, corev1.TaintEffectNoSchedule, []*corev1.Pod{pod})
	clock := clocktesting.NewFakeClock(activity.Status.HeartbeatAt)
	r.Clock = clock
	r.Eviction.RetryBackoff = 17 * time.Second
	r.Eviction.RetryJitter = 5 * time.Second
	r.Eviction.StandardBudget = EvictionBudget{PerNode: 5, Global: 5, Interval: time.Minute, BurstPerNode: 5, BurstGlobal: 5}
	evictor.err = apierrors.NewTooManyRequests("PDB blocks eviction", 45)
	delay := blockedRetryDelay(retryKey(activity.Spec.NodeName, pod), r.Eviction.normalized(), evictor.err)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: activity.Name, Namespace: activity.Namespace}}
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(evictor.names) != 1 {
		t.Fatalf("attempts=%d", len(evictor.names))
	}
	clock.Step(17 * time.Second)
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(evictor.names) != 1 {
		t.Fatal("configured backoff ignored the longer API hint")
	}
	clock.Step(delay - 17*time.Second - time.Nanosecond)
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(evictor.names) != 1 {
		t.Fatal("retried before hinted deadline")
	}
	clock.Step(time.Nanosecond)
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(evictor.names) != 2 {
		t.Fatalf("attempts at hinted deadline=%d", len(evictor.names))
	}
}

func TestEvictionRetryPrunesExpiredAndDisappearedPods(t *testing.T) {
	now := time.Now()
	var retries evictionRetries
	live := eligiblePod("live")
	live.UID = "live"
	key := retryKey("desktop", live)
	retries.block(key, now.Add(time.Minute))
	retries.block(evictionRetryKey{node: "desktop", uid: "disappeared"}, now.Add(time.Minute))
	retries.block(evictionRetryKey{node: "removed-node", uid: "expired"}, now.Add(-time.Second))
	retries.prune(now, "desktop", []corev1.Pod{*live})
	if len(retries.deadlines) != 1 || retries.remaining(key, now) != time.Minute {
		t.Fatalf("deadlines=%#v", retries.deadlines)
	}
	retries.prune(now, "desktop", nil)
	if len(retries.deadlines) != 0 {
		t.Fatalf("inactive node retained deadlines: %#v", retries.deadlines)
	}
}
