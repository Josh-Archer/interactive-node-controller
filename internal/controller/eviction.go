package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	EvictionCondition = "Eviction"
	EvictableLabel    = "interactive-node-controller.io/evictable"
	PinnedAnnotation  = "interactive-node-controller.io/pinned"

	AllowLossyEvictionAnnotation = "interactive-node-controller.io/allow-lossy-eviction"
	AllowLossyEvictionLabel      = "interactive-node-controller.io/allow-lossy-eviction"

	evictionOutcomeAttempted = "attempted"
	evictionOutcomeAudited   = "audited"
	evictionOutcomeEvicted   = "evicted"
	evictionOutcomeSkipped   = "skipped"
	evictionOutcomeBlocked   = "blocked"

	evictionStateDisabled      = "disabled"
	evictionStateAuditOnly     = "audit-only"
	evictionStateInactive      = "inactive"
	evictionStateNoEligible    = "no-eligible-pods"
	evictionStateBlocked       = "blocked"
	evictionStateBudgetWaiting = "budget-waiting"
	evictionStateCompleted     = "completed"

	EvictionReasonDisabled      = "EvictionDisabled"
	EvictionReasonAuditOnly     = "AuditOnly"
	EvictionReasonInactive      = "EvictionInactive"
	EvictionReasonNoEligible    = "NoEligiblePods"
	EvictionReasonBlocked       = "EvictionBlocked"
	EvictionReasonBudgetWaiting = "BudgetWaiting"
	EvictionReasonCompleted     = "EvictionCompleted"
)

func isLossyEvictionAllowed(pod *corev1.Pod) bool {
	if pod.Annotations != nil && strings.EqualFold(pod.Annotations[AllowLossyEvictionAnnotation], "true") {
		return true
	}
	if pod.Labels != nil && strings.EqualFold(pod.Labels[AllowLossyEvictionLabel], "true") {
		return true
	}
	return false
}

var evictionOutcomes = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "interactive_node_controller_evictions_total",
	Help: "Eviction candidates and outcomes by safety decision.",
}, []string{"outcome", "reason"})

func init() {
	metrics.Registry.MustRegister(evictionOutcomes)
}

// EvictionClient is intentionally narrower than a Kubernetes clientset. The
// controller can create only a policy/v1 Eviction, never a Pod deletion.
type EvictionClient interface {
	Evict(context.Context, string, string, metav1.DeleteOptions) error
}

type KubernetesEvictionClient struct{ Client kubernetes.Interface }

func (c KubernetesEvictionClient) Evict(ctx context.Context, namespace, name string, options metav1.DeleteOptions) error {
	return c.Client.PolicyV1().Evictions(namespace).Evict(ctx, &policyv1.Eviction{
		ObjectMeta:    metav1.ObjectMeta{Name: name, Namespace: namespace},
		DeleteOptions: &options,
	})
}

// EvictionPolicy is fail-safe by default. Enabled must be true and Audit must
// be false before the Eviction API is called.
type EvictionPolicy struct {
	Enabled             bool
	Audit               bool
	ProtectedNamespaces map[string]struct{}
	// MaxPerReconcile is the default steady allowance for both classes when a
	// class-specific budget field is zero. It is applied once per budget
	// interval, not once per reconcile. The flag name is historical.
	MaxPerReconcile int
	// BudgetInterval is the default refill window. Zero defaults to one minute.
	BudgetInterval time.Duration
	RetryBackoff   time.Duration
	// RetryJitter is the maximum stable per-Pod-UID delay added to a blocked
	// retry. Zero disables jitter. Values above RetryBackoff are capped at the
	// backoff so the extra wait stays bounded.
	RetryJitter    time.Duration
	EvictableLabel string
	// StandardBudget and LossyBudget are independent. Lossy opt-in does not
	// grant a hard-coded minimum; unset lossy fields use the same conservative
	// defaults as standard pods.
	StandardBudget EvictionBudget
	LossyBudget    EvictionBudget
}

func (p EvictionPolicy) normalized() EvictionPolicy {
	if p.MaxPerReconcile <= 0 {
		p.MaxPerReconcile = 1
	}
	if p.BudgetInterval <= 0 {
		p.BudgetInterval = time.Minute
	}
	if p.RetryBackoff <= 0 {
		p.RetryBackoff = 30 * time.Second
	}
	if p.RetryJitter < 0 {
		p.RetryJitter = 0
	}
	if p.RetryJitter > p.RetryBackoff {
		p.RetryJitter = p.RetryBackoff
	}
	if strings.TrimSpace(p.EvictableLabel) == "" {
		p.EvictableLabel = EvictableLabel
	}
	if p.ProtectedNamespaces == nil {
		p.ProtectedNamespaces = map[string]struct{}{}
	}
	p.StandardBudget = p.StandardBudget.normalized(p.MaxPerReconcile, p.BudgetInterval)
	p.LossyBudget = p.LossyBudget.normalized(p.MaxPerReconcile, p.BudgetInterval)
	return p
}

func (p EvictionPolicy) Validate() error {
	p = p.normalized()
	if strings.TrimSpace(p.EvictableLabel) == "" || strings.ContainsAny(p.EvictableLabel, " \t\n") {
		return fmt.Errorf("evictable-label is required and cannot contain whitespace")
	}
	if p.MaxPerReconcile <= 0 {
		return fmt.Errorf("max-evictions-per-reconcile must be positive")
	}
	if p.RetryBackoff <= 0 {
		return fmt.Errorf("eviction-retry-backoff must be positive")
	}
	if p.BudgetInterval <= 0 || p.StandardBudget.Interval <= 0 || p.LossyBudget.Interval <= 0 {
		return fmt.Errorf("eviction budget interval must be positive")
	}
	if p.StandardBudget.PerNode <= 0 || p.StandardBudget.Global <= 0 || p.LossyBudget.PerNode <= 0 || p.LossyBudget.Global <= 0 {
		return fmt.Errorf("eviction budgets must be positive")
	}
	if p.StandardBudget.BurstPerNode <= 0 || p.StandardBudget.BurstGlobal <= 0 || p.LossyBudget.BurstPerNode <= 0 || p.LossyBudget.BurstGlobal <= 0 {
		return fmt.Errorf("eviction burst allowances must be positive")
	}
	return nil
}

type evictionSummary struct {
	audited, attempted, evicted, skipped, blocked, deferred int
	outcome                                                 string
	blockerReason                                           string
	requeueAfter                                            time.Duration
	message                                                 string
}

func (s evictionSummary) condition(now time.Time, generation int64) metav1.Condition {
	status := metav1.ConditionTrue
	reason := EvictionReasonDisabled
	message := s.message
	switch s.outcome {
	case evictionStateAuditOnly:
		reason = EvictionReasonAuditOnly
	case evictionStateInactive:
		reason = EvictionReasonInactive
	case evictionStateNoEligible:
		reason = EvictionReasonNoEligible
	case evictionStateBlocked:
		status, reason = metav1.ConditionFalse, EvictionReasonBlocked
	case evictionStateBudgetWaiting:
		reason = EvictionReasonBudgetWaiting
	case evictionStateCompleted:
		reason = EvictionReasonCompleted
	}
	if message == "" {
		message = "eviction is disabled"
	}
	return metav1.Condition{Type: EvictionCondition, Status: status, Reason: reason, Message: message, ObservedGeneration: generation, LastTransitionTime: metav1.NewTime(now)}
}

func (s *evictionSummary) noteBlocker(reason, message string) {
	if blockerRank(reason) >= blockerRank(s.blockerReason) {
		s.blockerReason = reason
		if message != "" {
			s.message = message
		}
	}
}

func blockerRank(reason string) int {
	switch reason {
	case "retry-backoff":
		return 1
	case "pod-gone":
		return 2
	case "eviction-conflict":
		return 3
	case "api-error":
		return 4
	case "pdb-blocked":
		return 5
	default:
		return 0
	}
}

func (s *evictionSummary) classify() {
	switch {
	case s.blocked > 0:
		s.outcome = evictionStateBlocked
		if s.message == "" {
			s.message = "eviction is blocked"
		}
	case s.deferred > 0:
		// Eligible Pods remain and the reconcile is requeued for budget refill.
		s.outcome = evictionStateBudgetWaiting
		otherSkipped := s.skipped - s.deferred
		if otherSkipped < 0 {
			otherSkipped = 0
		}
		s.message = fmt.Sprintf("evicted %d eligible Pod(s); skipped %d; %d eligible Pod(s) waiting for eviction budget", s.evicted, otherSkipped, s.deferred)
	case s.attempted > 0:
		s.outcome = evictionStateCompleted
		if s.message == "" {
			s.message = fmt.Sprintf("evicted %d eligible Pod(s); skipped %d", s.evicted, s.skipped)
		}
	case s.audited > 0:
		s.outcome = evictionStateAuditOnly
		if s.message == "" {
			s.message = fmt.Sprintf("audit only: %d eligible Pod(s); skipped %d", s.audited, s.skipped)
		}
	default:
		s.outcome = evictionStateNoEligible
		if s.message == "" {
			s.message = fmt.Sprintf("no eligible Pods; skipped %d", s.skipped)
		}
	}
}

func boundedEvictionReason(reason string) string {
	switch reason {
	case "not-opted-in", "protected-namespace", "terminating", "mirror-static", "daemonset-owned",
		"unmanaged-pod", "direct-pinned", "node-selector", "required-node-affinity", "critical-priority",
		"hostpath-volume", "local-emptydir", "pvc-lookup-failed", "pvc-not-bound", "pvc-rwo",
		"pv-lookup-failed", "local-pv", "pvc-not-allowed", "completed-pod", "eligible", "audit",
		"rate-limited", "missing-pod-identity", "api-error", "pdb-blocked", "eviction-conflict",
		"pod-gone", "accepted", "retry-backoff":
		return reason
	default:
		return "unclassified"
	}
}

func boundedBlockerReason(reason string) string {
	switch reason {
	case "pdb-blocked", "api-error", "eviction-conflict", "pod-gone", "retry-backoff":
		return reason
	default:
		return "api-error"
	}
}

// evictionClass is the safety contract for one live enrollment. Duplicate
// enrollments never reach it: Reconcile skips eviction when more than one
// NodeActivity targets the Node. docs/controller.md is the same matrix.
type evictionClass int

const (
	evictionClassNone evictionClass = iota
	evictionClassGame
	evictionClassLossyOnly
)

func (r *NodeActivityReconciler) evictionClass(activity *availabilityv1alpha1.NodeActivity, node *corev1.Node, desired *corev1.Taint, now time.Time) evictionClass {
	verdict := heartbeatFreshness(activity.Status.HeartbeatAt, now, r.Policy.StaleAfter, r.Policy.effectiveHeartbeatSkew())
	// An implausible future timestamp is not evidence the host is gone.
	// Fail-closed scheduling still applies. The timestamp does not authorize
	// eviction, even when the reporter state is stale.
	if verdict.Future {
		return evictionClassNone
	}
	fresh := verdict.Fresh
	staleHeartbeat := verdict.Missing || verdict.Expired

	// Fresh unknown is a startup or debounce report. Fail-closed and burst
	// NoSchedule fences still apply, and neither fence authorizes eviction.
	if fresh && activity.Status.State == availabilityv1alpha1.StateUnknown {
		return evictionClassNone
	}
	// Fresh idle with no desired taint is standing down, including a scheduled
	// idle-allowed window. A leftover burst fence is not an eviction signal.
	if fresh && desired == nil &&
		activity.Status.State == availabilityv1alpha1.StateIdle &&
		activity.Status.Activity == availabilityv1alpha1.ActivityIdle {
		return evictionClassNone
	}

	// An expired heartbeat is the stale row, even if the last report was an
	// active game and the active NoSchedule taint has not been replaced yet.
	if fresh &&
		activity.Status.State == availabilityv1alpha1.StateActive &&
		activity.Status.Activity == availabilityv1alpha1.ActivityGame &&
		desired != nil && desired.Effect == corev1.TaintEffectNoSchedule && desired.Value == r.Policy.ActiveValue &&
		hasTaint(node, r.Policy.Key, r.Policy.ActiveValue, corev1.TaintEffectNoSchedule) {
		return evictionClassGame
	}

	if (staleHeartbeat || activity.Status.State == availabilityv1alpha1.StateStale) &&
		desired != nil && desired.Effect == corev1.TaintEffectNoSchedule &&
		hasTaint(node, r.Policy.Key, desired.Value, corev1.TaintEffectNoSchedule) {
		return evictionClassLossyOnly
	}

	// A stale heartbeat evicts only through the fail-closed row above. With
	// fail-closed disabled (or its NoSchedule not yet applied) nothing evicts.
	if staleHeartbeat || activity.Status.State == availabilityv1alpha1.StateStale {
		return evictionClassNone
	}
	// No desired taint means the node admits new Pods (for example a scheduled
	// idle-allowed window). Evicting while admitting only causes churn.
	if desired == nil {
		return evictionClassNone
	}

	// Burst NoSchedule is a scheduling fence on every node that is not fresh
	// idle. It authorizes lossy eviction only after the fresh-unknown and
	// fresh-idle stand-down cases above have been excluded.
	if (activity.Status.State == availabilityv1alpha1.StateActive && activity.Status.Activity != availabilityv1alpha1.ActivityGame) ||
		activity.Status.Activity == availabilityv1alpha1.ActivityInteractive ||
		hasTaint(node, r.Policy.Key, r.Policy.InteractiveValue, corev1.TaintEffectPreferNoSchedule) ||
		(r.Policy.BurstTaintKey != "" && hasTaint(node, r.Policy.BurstTaintKey, r.Policy.FailClosedValue, corev1.TaintEffectNoSchedule)) {
		return evictionClassLossyOnly
	}
	return evictionClassNone
}

func (r *NodeActivityReconciler) reconcileEvictions(ctx context.Context, activity *availabilityv1alpha1.NodeActivity, node *corev1.Node, desired *corev1.Taint) (evictionSummary, error) {
	policy := r.Eviction.normalized()
	summary := evictionSummary{}
	now := r.clock().Now()
	if !policy.Enabled && !policy.Audit {
		r.retries.prune(now, node.Name, nil)
		summary.outcome = evictionStateDisabled
		summary.message = "eviction is disabled"
		return summary, nil
	}
	class := r.evictionClass(activity, node, desired, now)
	if class == evictionClassNone {
		r.retries.prune(now, node.Name, nil)
		summary.outcome = evictionStateInactive
		summary.message = "eviction policy is inactive for the current node state"
		return summary, nil
	}

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.MatchingFields{PodNodeNameKey: node.Name}); err != nil {
		return summary, err
	}
	r.retries.prune(now, node.Name, pods.Items)
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName != node.Name {
			continue
		}
		allowLossy := isLossyEvictionAllowed(pod)
		switch class {
		case evictionClassGame:
		case evictionClassLossyOnly:
			if !allowLossy {
				continue
			}
		default:
			continue
		}
		decision, reason := r.evictionDecision(ctx, pod, node.Name, policy)
		key := retryKey(node.Name, pod)
		if !decision {
			r.retries.forget(key)
			summary.skipped++
			observeEviction(evictionOutcomeSkipped, reason)
			continue
		}
		if policy.Audit || !policy.Enabled || r.Evictor == nil {
			r.retries.forget(key)
			summary.audited++
			observeEviction(evictionOutcomeAudited, "audit")
			continue
		}
		if remaining := r.retries.remaining(key, now); remaining > 0 {
			summary.blocked++
			summary.noteBlocker("retry-backoff", "eviction retry backoff is pending")
			summary.retryAfter(remaining)
			continue
		}
		// Bind the request to the exact object that passed the safety gates.
		// A same-name replacement or metadata change must be re-evaluated.
		// Missing identity does not spend budget.
		uid, resourceVersion := pod.UID, pod.ResourceVersion
		if uid == "" || resourceVersion == "" {
			summary.skipped++
			observeEviction(evictionOutcomeSkipped, "missing-pod-identity")
			continue
		}
		// Per-node and global buckets are time-based. A Pod still inside its
		// retry deadline never reaches this spend. Refused candidates are
		// requeued for the moment both buckets have a token again.
		if ok, wait := r.budgets.tryTake(allowLossy, node.Name, now, policy); !ok {
			summary.skipped++
			summary.deferred++
			observeEviction(evictionOutcomeSkipped, "rate-limited")
			summary.retryAfter(wait)
			continue
		}
		summary.attempted++
		observeEviction(evictionOutcomeAttempted, "eligible")
		options := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &resourceVersion}}
		if pod.Spec.TerminationGracePeriodSeconds != nil {
			grace := *pod.Spec.TerminationGracePeriodSeconds
			options.GracePeriodSeconds = &grace
		}
		if err := r.Evictor.Evict(ctx, pod.Namespace, pod.Name, options); err != nil {
			summary.blocked++
			reason := "api-error"
			if apierrors.IsTooManyRequests(err) {
				reason = "pdb-blocked"
			} else if apierrors.IsConflict(err) {
				reason = "eviction-conflict"
			} else if apierrors.IsNotFound(err) {
				reason = "pod-gone"
			}
			observeEviction(evictionOutcomeBlocked, reason)
			summary.noteBlocker(reason, fmt.Sprintf("eviction blocked for %s/%s (%s); retrying", pod.Namespace, pod.Name, reason))
			delay := blockedRetryDelay(key, policy, err)
			r.retries.block(key, now.Add(delay))
			summary.retryAfter(delay)
			continue
		}
		summary.evicted++
		r.retries.forget(key)
		observeEviction(evictionOutcomeEvicted, "accepted")
		if summary.blocked == 0 {
			summary.message = fmt.Sprintf("evicted %d eligible Pod(s); skipped %d", summary.evicted, summary.skipped)
		}
	}
	if summary.message == "" {
		if summary.audited > 0 {
			summary.message = fmt.Sprintf("audit only: %d eligible Pod(s); skipped %d", summary.audited, summary.skipped)
		} else if summary.deferred > 0 {
			summary.message = fmt.Sprintf("eviction budget exhausted; %d eligible Pod(s) waiting for eviction budget", summary.deferred)
		} else {
			summary.message = fmt.Sprintf("no eligible Pods; skipped %d", summary.skipped)
		}
	} else if summary.deferred > 0 && !strings.Contains(summary.message, "waiting for eviction budget") {
		summary.message = fmt.Sprintf("%s; %d eligible Pod(s) waiting for eviction budget", summary.message, summary.deferred)
	}
	summary.classify()
	return summary, nil
}

func (r *NodeActivityReconciler) evictionDecision(ctx context.Context, pod *corev1.Pod, nodeName string, policy EvictionPolicy) (bool, string) {
	if pod.Labels[policy.EvictableLabel] != "true" {
		return false, "not-opted-in"
	}
	if _, protected := policy.ProtectedNamespaces[pod.Namespace]; protected {
		return false, "protected-namespace"
	}
	if pod.DeletionTimestamp != nil {
		return false, "terminating"
	}
	if pod.Annotations != nil && pod.Annotations[corev1.MirrorPodAnnotationKey] != "" {
		return false, "mirror-static"
	}
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "DaemonSet" {
			return false, "daemonset-owned"
		}
	}
	if len(pod.OwnerReferences) == 0 {
		return false, "unmanaged-pod"
	}
	if pod.Annotations != nil && strings.EqualFold(pod.Annotations[PinnedAnnotation], "true") {
		return false, "direct-pinned"
	}
	allowLossy := isLossyEvictionAllowed(pod)
	if len(pod.Spec.NodeSelector) > 0 && !allowLossy {
		// A selector may target a scarce capability or a single node. Without
		// proving scheduler feasibility across the cluster, treat every selector
		// as ambiguous and leave the Pod in place.
		return false, "node-selector"
	}
	if pod.Spec.Affinity != nil && pod.Spec.Affinity.NodeAffinity != nil && pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil && !allowLossy {
		return false, "required-node-affinity"
	}
	if pod.Spec.Priority != nil && *pod.Spec.Priority >= systemPriority {
		return false, "critical-priority"
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.HostPath != nil {
			return false, "hostpath-volume"
		}
		if volume.EmptyDir != nil && !allowLossy {
			return false, "local-emptydir"
		}
		if volume.PersistentVolumeClaim == nil {
			continue
		}
		pvc := &corev1.PersistentVolumeClaim{}
		ref := types.NamespacedName{Name: volume.PersistentVolumeClaim.ClaimName, Namespace: pod.Namespace}
		if err := r.Get(ctx, ref, pvc); err != nil {
			return false, "pvc-lookup-failed"
		}
		if pvc.Spec.VolumeName == "" {
			return false, "pvc-not-bound"
		}
		for _, mode := range pvc.Spec.AccessModes {
			if mode == corev1.ReadWriteOnce || mode == corev1.ReadWriteOncePod {
				return false, "pvc-rwo"
			}
		}
		pv := &corev1.PersistentVolume{}
		if err := r.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, pv); err != nil {
			return false, "pv-lookup-failed"
		}
		if pv.Spec.Local != nil || pv.Spec.HostPath != nil {
			return false, "local-pv"
		}
		// Bound ReadWriteMany and ReadOnlyMany stay eligible for standard pods.
		// Lossy opt-in does not extend to any PersistentVolumeClaim.
		if allowLossy {
			return false, "pvc-not-allowed"
		}
	}
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return false, "completed-pod"
	}
	return true, "eligible"
}

const systemPriority int32 = 2000000000

func hasTaint(node *corev1.Node, key, value string, effect corev1.TaintEffect) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == key && taint.Value == value && taint.Effect == effect {
			return true
		}
	}
	return false
}

func observeEviction(outcome, reason string) {
	evictionOutcomes.WithLabelValues(outcome, boundedEvictionReason(reason)).Inc()
}
