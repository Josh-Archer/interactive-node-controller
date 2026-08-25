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
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	EvictionCondition = "Eviction"
	EvictableLabel    = "interactive-node-controller.io/evictable"
	PinnedAnnotation  = "interactive-node-controller.io/pinned"

	evictionOutcomeAttempted = "attempted"
	evictionOutcomeAudited   = "audited"
	evictionOutcomeEvicted   = "evicted"
	evictionOutcomeSkipped   = "skipped"
	evictionOutcomeBlocked   = "blocked"
)

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
	MaxPerReconcile     int
	RetryBackoff        time.Duration
	EvictableLabel      string
}

func (p EvictionPolicy) normalized() EvictionPolicy {
	if p.MaxPerReconcile <= 0 {
		p.MaxPerReconcile = 1
	}
	if p.RetryBackoff <= 0 {
		p.RetryBackoff = 30 * time.Second
	}
	if strings.TrimSpace(p.EvictableLabel) == "" {
		p.EvictableLabel = EvictableLabel
	}
	if p.ProtectedNamespaces == nil {
		p.ProtectedNamespaces = map[string]struct{}{}
	}
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
	return nil
}

type evictionSummary struct {
	audited, attempted, evicted, skipped, blocked int
	requeueAfter                                  time.Duration
	message                                       string
}

func (s evictionSummary) condition(now time.Time, generation int64) metav1.Condition {
	status := metav1.ConditionTrue
	reason := "EvictionDisabled"
	message := "eviction is disabled"
	if s.blocked > 0 {
		status, reason = metav1.ConditionFalse, "EvictionBlocked"
		message = s.message
	} else if s.audited > 0 && s.attempted == 0 {
		reason = "AuditOnly"
		message = s.message
	} else if s.attempted > 0 {
		reason = "EvictionReconciled"
		message = s.message
	}
	return metav1.Condition{Type: EvictionCondition, Status: status, Reason: reason, Message: message, ObservedGeneration: generation, LastTransitionTime: metav1.NewTime(now)}
}

func (r *NodeActivityReconciler) reconcileEvictions(ctx context.Context, activity *availabilityv1alpha1.NodeActivity, node *corev1.Node, desired *corev1.Taint) (evictionSummary, error) {
	policy := r.Eviction.normalized()
	summary := evictionSummary{}
	if !policy.Enabled && !policy.Audit {
		return summary, nil
	}
	if activity.Status.State != availabilityv1alpha1.StateActive || activity.Status.Activity != availabilityv1alpha1.ActivityGame || !hasTaint(node, r.Policy.Key, r.Policy.ActiveValue, corev1.TaintEffectNoSchedule) || desired == nil || desired.Effect != corev1.TaintEffectNoSchedule || desired.Value != r.Policy.ActiveValue {
		return summary, nil
	}

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods); err != nil {
		return summary, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName != node.Name {
			continue
		}
		decision, reason := r.evictionDecision(ctx, pod, node.Name, policy)
		if !decision {
			summary.skipped++
			observeEviction(evictionOutcomeSkipped, reason)
			continue
		}
		if policy.Audit || !policy.Enabled || r.Evictor == nil {
			summary.audited++
			observeEviction(evictionOutcomeAudited, "audit")
			continue
		}
		if summary.attempted >= policy.MaxPerReconcile {
			summary.skipped++
			observeEviction(evictionOutcomeSkipped, "rate-limited")
			continue
		}
		summary.attempted++
		observeEviction(evictionOutcomeAttempted, "eligible")
		options := metav1.DeleteOptions{}
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
			summary.message = fmt.Sprintf("eviction blocked for %s/%s (%s); retrying", pod.Namespace, pod.Name, reason)
			summary.requeueAfter = policy.RetryBackoff
			continue
		}
		summary.evicted++
		observeEviction(evictionOutcomeEvicted, "accepted")
		summary.message = fmt.Sprintf("evicted %d eligible Pod(s); skipped %d", summary.evicted, summary.skipped)
	}
	if summary.message == "" {
		if summary.audited > 0 {
			summary.message = fmt.Sprintf("audit only: %d eligible Pod(s); skipped %d", summary.audited, summary.skipped)
		} else {
			summary.message = fmt.Sprintf("no eligible Pods; skipped %d", summary.skipped)
		}
	}
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
	if pod.Spec.NodeSelector != nil && pod.Spec.NodeSelector["kubernetes.io/hostname"] == nodeName {
		return false, "host-pinned-selector"
	}
	if pod.Spec.Affinity != nil && pod.Spec.Affinity.NodeAffinity != nil && pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		return false, "required-node-affinity"
	}
	if pod.Spec.Priority != nil && *pod.Spec.Priority >= systemPriority {
		return false, "critical-priority"
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.HostPath != nil {
			return false, "hostpath-volume"
		}
		if volume.EmptyDir != nil {
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

func observeEviction(outcome, reason string) { evictionOutcomes.WithLabelValues(outcome, reason).Inc() }
