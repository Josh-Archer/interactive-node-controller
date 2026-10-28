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
	taint            *corev1.Taint
	source           *availabilityv1alpha1.NodeActivity
	reason           string
	admissionReason  string
	requeueAfter     time.Duration
	count            int
	scheduled        bool
	nextWindow       time.Time
	nextTransition   time.Time
	transitionReason string
}

// Aggregate every live enrollment for this Node across namespaces. The
// spec.nodeName index keeps the lookup proportional to matching enrollments.
// Ordering makes equal contributions deterministic without process-local
// ownership or winner state.
func (r *NodeActivityReconciler) protectionForNode(ctx context.Context, nodeName string, now time.Time) (enrollmentProtection, error) {
	var enrollments availabilityv1alpha1.NodeActivityList
	if err := r.List(ctx, &enrollments, client.MatchingFields{NodeActivityNodeNameKey: nodeName}); err != nil {
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
		decision, err := r.decidePolicy(item, now)
		if err != nil {
			return enrollmentProtection{}, err
		}
		if decision.requeueAfter > 0 && (result.requeueAfter == 0 || decision.requeueAfter < result.requeueAfter) {
			result.requeueAfter = decision.requeueAfter
		}
		rank := 0
		if decision.taint != nil {
			rank = 1
			if decision.taint.Effect == corev1.TaintEffectNoSchedule {
				rank = 2
				if item.Status.State == availabilityv1alpha1.StateActive && item.Status.Activity == availabilityv1alpha1.ActivityGame && heartbeatFreshness(item.Status.HeartbeatAt, now, r.Policy.StaleAfter, r.Policy.effectiveHeartbeatSkew()).Fresh {
					rank = 3
				}
			}
		}
		if rank > best {
			best = rank
			result.taint = decision.taint
			result.source = item
			result.reason = decision.message
			result.admissionReason = decision.admissionReason
			result.scheduled = decision.scheduled
			result.nextWindow = decision.nextWindow
			result.nextTransition = decision.nextTransition
			result.transitionReason = decision.transitionReason
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
	if err := r.List(ctx, &enrollments, client.MatchingFields{NodeActivityNodeNameKey: activity.Spec.NodeName}); err != nil {
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
