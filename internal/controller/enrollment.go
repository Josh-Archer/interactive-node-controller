package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type enrollmentProtection struct {
	taint        *corev1.Taint
	source       *availabilityv1alpha1.NodeActivity
	reason       string
	requeueAfter time.Duration
	count        int
}

// Aggregate every live enrollment across namespaces. Ordering makes equal
// contributions deterministic without process-local ownership or winner state.
func (r *NodeActivityReconciler) protectionForNode(ctx context.Context, nodeName string, now time.Time) (enrollmentProtection, error) {
	var enrollments availabilityv1alpha1.NodeActivityList
	if err := r.List(ctx, &enrollments); err != nil {
		return enrollmentProtection{}, err
	}
	sort.Slice(enrollments.Items, func(i, j int) bool {
		a, b := enrollments.Items[i], enrollments.Items[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	result := enrollmentProtection{}
	best := -1
	for i := range enrollments.Items {
		item := &enrollments.Items[i]
		if item.Spec.NodeName != nodeName || !item.DeletionTimestamp.IsZero() {
			continue
		}
		result.count++
		taint, reason, delay := r.desiredTaint(item, now)
		if delay > 0 && (result.requeueAfter == 0 || delay < result.requeueAfter) {
			result.requeueAfter = delay
		}
		rank := 0
		if taint != nil {
			rank = 1
			if taint.Effect == corev1.TaintEffectNoSchedule {
				rank = 2
				if item.Status.State == availabilityv1alpha1.StateActive && item.Status.Activity == availabilityv1alpha1.ActivityGame && !item.Status.HeartbeatAt.IsZero() && now.Sub(item.Status.HeartbeatAt) < r.Policy.StaleAfter {
					rank = 3
				}
			}
		}
		if rank > best {
			best = rank
			result.taint, result.source, result.reason = taint, item, reason
		}
	}
	if result.count > 1 {
		result.reason = fmt.Sprintf("%d live NodeActivity enrollments target this Node; strongest protection applied; remove duplicate enrollments to enable eviction", result.count)
	}
	return result, nil
}

// Changes to one enrollment must also refresh surviving enrollments' conditions.
func (r *NodeActivityReconciler) requestsForNode(ctx context.Context, object client.Object) []ctrl.Request {
	activity, ok := object.(*availabilityv1alpha1.NodeActivity)
	if !ok || activity.Spec.NodeName == "" {
		return nil
	}
	var enrollments availabilityv1alpha1.NodeActivityList
	if err := r.List(ctx, &enrollments); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "list sibling NodeActivity enrollments")
		return nil
	}
	var requests []ctrl.Request
	for _, item := range enrollments.Items {
		if item.Spec.NodeName == activity.Spec.NodeName {
			requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: item.Namespace, Name: item.Name}})
		}
	}
	return requests
}
