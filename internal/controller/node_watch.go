package controller

import (
	"context"
	"reflect"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// requestsForEnrolledNode maps a Node event onto every NodeActivity that enrolls it.
// Reconcile then restores a managed taint or schedulable label that changed outside
// this controller, without waiting for the next reporter status write.
func (r *NodeActivityReconciler) requestsForEnrolledNode(ctx context.Context, object client.Object) []ctrl.Request {
	node, ok := object.(*corev1.Node)
	if !ok || node.Name == "" {
		return nil
	}
	var enrollments availabilityv1alpha1.NodeActivityList
	if err := r.List(ctx, &enrollments, client.MatchingFields{NodeActivityNodeNameKey: node.Name}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "list NodeActivity enrollments for node watch", "node", node.Name)
		return nil
	}
	var requests []ctrl.Request
	for _, item := range enrollments.Items {
		if item.Spec.NodeName == node.Name {
			requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: item.Namespace, Name: item.Name}})
		}
	}
	return requests
}

// nodeOwnedFieldsChanged accepts Node events that can drift fields this controller owns.
// Kubelet status heartbeats leave those fields unchanged and do not enqueue work.
func (r *NodeActivityReconciler) nodeOwnedFieldsChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldNode, okOld := e.ObjectOld.(*corev1.Node)
			newNode, okNew := e.ObjectNew.(*corev1.Node)
			if !okOld || !okNew {
				return false
			}
			if schedulableLabelValue(oldNode) != schedulableLabelValue(newNode) {
				return true
			}
			return !reflect.DeepEqual(managedTaintProjection(oldNode, r.Policy), managedTaintProjection(newNode, r.Policy))
		},
	}
}

func schedulableLabelValue(node *corev1.Node) string {
	if node.Labels == nil {
		return ""
	}
	return node.Labels[SchedulableNodeLabel]
}

// managedTaintProjection keeps key, value, and effect for taints this policy owns.
// TimeAdded is omitted so an API-populated timestamp cannot requeue forever.
func managedTaintProjection(node *corev1.Node, policy TaintPolicy) []corev1.Taint {
	keys := map[string]struct{}{}
	if policy.Key != "" {
		keys[policy.Key] = struct{}{}
	}
	if policy.BurstTaintKey != "" {
		keys[policy.BurstTaintKey] = struct{}{}
	}
	var found []corev1.Taint
	for _, taint := range node.Spec.Taints {
		if _, ok := keys[taint.Key]; !ok {
			continue
		}
		found = append(found, corev1.Taint{Key: taint.Key, Value: taint.Value, Effect: taint.Effect})
	}
	return found
}
