package controller

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"time"
)

// nodeReadyProjection deliberately excludes LastHeartbeatTime so routine kubelet
// heartbeats do not enqueue reconciles. Transition time gates post-outage reports.
func nodeReadyProjection(node *corev1.Node) (corev1.ConditionStatus, metav1.Time) {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status, condition.LastTransitionTime
		}
	}
	return corev1.ConditionUnknown, metav1.Time{}
}

// withNodeReadiness never grants eviction authority. It fences admission while
// Kubernetes cannot confirm readiness or the reporter has not observed recovery.
// It leaves unrelated/native taints, pod identity and workload specs untouched.
func (r *NodeActivityReconciler) withNodeReadiness(p enrollmentProtection, node *corev1.Node) enrollmentProtection {
	if p.source == nil {
		return p
	} // final enrollment deletion still cleans up
	status, transition := nodeReadyProjection(node)
	reason, message := "", ""
	switch {
	case status != corev1.ConditionTrue:
		reason, message = "node-not-ready", "Kubernetes Node Ready is not True; admission fenced (not workload recovery)"
	case transition.IsZero() || (!p.source.Status.HeartbeatAt.IsZero() && p.source.Status.HeartbeatAt.Before(transition.Time)):
		reason, message = "node-recovery-pending", "waiting for a reporter heartbeat at or after the Node Ready transition; admission fenced"
	}
	if reason == "" {
		return p
	}
	p.taint = &corev1.Taint{Key: r.Policy.Key, Value: r.Policy.FailClosedValue, Effect: corev1.TaintEffectNoSchedule}
	p.admissionReason, p.reason = reason, message
	// Poll as well as watching NodeActivity/Node changes, including clock recovery.
	if p.requeueAfter <= 0 || p.requeueAfter > 30*time.Second {
		p.requeueAfter = 30 * time.Second
	}
	return p
}
