// Package controller reconciles host state into one explicitly owned Node taint.
package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const TaintAppliedCondition = "TaintApplied"

// TaintPolicy is deliberately small: one controller instance owns one taint
// key. It never alters unrelated taints or workload objects.
type TaintPolicy struct {
	Key              string
	InteractiveValue string
	ActiveValue      string
	FailClosedValue  string
	StaleAfter       time.Duration
	FailClosed       bool
}

func (p TaintPolicy) Validate() error {
	if strings.TrimSpace(p.Key) == "" || strings.ContainsAny(p.Key, " \t\n") {
		return fmt.Errorf("taint key is required and cannot contain whitespace")
	}
	if p.InteractiveValue == "" || p.ActiveValue == "" || p.FailClosedValue == "" {
		return fmt.Errorf("all taint values are required")
	}
	if p.StaleAfter <= 0 {
		return fmt.Errorf("stale-after must be positive")
	}
	return nil
}

// NodeActivityReconciler is the sole writer for its configured Node taint.
// It does not create, delete, or mutate Pods or workload specifications.
type NodeActivityReconciler struct {
	client.Client
	Policy   TaintPolicy
	Eviction EvictionPolicy
	Evictor  EvictionClient
	Clock    clock.Clock
}

func (r *NodeActivityReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	activity := &availabilityv1alpha1.NodeActivity{}
	if err := r.Get(ctx, request.NamespacedName, activity); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if err := r.Policy.Validate(); err != nil {
		return ctrl.Result{}, err
	}
	if strings.TrimSpace(activity.Spec.NodeName) == "" {
		return ctrl.Result{}, r.setCondition(ctx, activity, metav1.ConditionFalse, "InvalidEnrollment", "spec.nodeName is required", nil)
	}

	now := r.clock().Now()
	desired, reason, requeueAfter := r.desiredTaint(activity, now)
	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: activity.Spec.NodeName}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: requeueAfter}, r.setCondition(ctx, activity, metav1.ConditionFalse, "NodeNotFound", "enrolled node does not exist", desired)
		}
		return ctrl.Result{}, err
	}

	changed := reconcileOwnedTaint(node, r.Policy.Key, desired)
	if changed {
		if err := r.Update(ctx, node); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.setCondition(ctx, activity, metav1.ConditionTrue, "TaintReconciled", reason, desired); err != nil {
		return ctrl.Result{}, err
	}
	evictionSummary, err := r.reconcileEvictions(ctx, activity, node, desired)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.setEvictionCondition(ctx, activity, evictionSummary); err != nil {
		return ctrl.Result{}, err
	}
	result := ctrl.Result{RequeueAfter: requeueAfter}
	if evictionSummary.requeueAfter > 0 && (result.RequeueAfter <= 0 || evictionSummary.requeueAfter < result.RequeueAfter) {
		result.RequeueAfter = evictionSummary.requeueAfter
	}
	return result, nil
}

func (r *NodeActivityReconciler) setEvictionCondition(ctx context.Context, activity *availabilityv1alpha1.NodeActivity, summary evictionSummary) error {
	before := activity.DeepCopy()
	meta.SetStatusCondition(&activity.Status.Conditions, summary.condition(r.clock().Now(), activity.Generation))
	if reflect.DeepEqual(before.Status, activity.Status) {
		return nil
	}
	return r.Status().Patch(ctx, activity, client.MergeFrom(before))
}

func (r *NodeActivityReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		For(&availabilityv1alpha1.NodeActivity{}).
		Complete(r)
}

func (r *NodeActivityReconciler) clock() clock.Clock {
	if r.Clock != nil {
		return r.Clock
	}
	return clock.RealClock{}
}

func (r *NodeActivityReconciler) desiredTaint(activity *availabilityv1alpha1.NodeActivity, now time.Time) (*corev1.Taint, string, time.Duration) {
	stale := activity.Status.HeartbeatAt.IsZero() || now.Sub(activity.Status.HeartbeatAt) >= r.Policy.StaleAfter
	if stale {
		return r.failClosed("host heartbeat is missing or stale")
	}
	remaining := activity.Status.HeartbeatAt.Add(r.Policy.StaleAfter).Sub(now)
	if remaining <= 0 {
		remaining = time.Second
	}
	switch activity.Status.State {
	case availabilityv1alpha1.StateIdle:
		return nil, "host is idle; managed taint removed", remaining
	case availabilityv1alpha1.StateActive:
		if activity.Status.Activity == availabilityv1alpha1.ActivityGame {
			return &corev1.Taint{Key: r.Policy.Key, Value: r.Policy.ActiveValue, Effect: corev1.TaintEffectNoSchedule}, "host is running an active game", remaining
		}
		if activity.Status.Activity == availabilityv1alpha1.ActivityInteractive {
			return &corev1.Taint{Key: r.Policy.Key, Value: r.Policy.InteractiveValue, Effect: corev1.TaintEffectPreferNoSchedule}, "host is interactive", remaining
		}
		return r.failClosed("active state has an unsupported activity")
	case availabilityv1alpha1.StateUnknown, availabilityv1alpha1.StateStale:
		return r.failClosed("host state is unknown or stale")
	default:
		return r.failClosed("host reported an unsupported state")
	}
}

func (r *NodeActivityReconciler) failClosed(message string) (*corev1.Taint, string, time.Duration) {
	if !r.Policy.FailClosed {
		return nil, message + "; fail-closed disabled", r.Policy.StaleAfter
	}
	return &corev1.Taint{Key: r.Policy.Key, Value: r.Policy.FailClosedValue, Effect: corev1.TaintEffectNoSchedule}, message + "; fail-closed taint applied", r.Policy.StaleAfter
}

func reconcileOwnedTaint(node *corev1.Node, key string, desired *corev1.Taint) bool {
	next := make([]corev1.Taint, 0, len(node.Spec.Taints)+1)
	for _, taint := range node.Spec.Taints {
		if taint.Key != key {
			next = append(next, taint)
		}
	}
	if desired != nil {
		next = append(next, *desired)
	}
	if len(next) == len(node.Spec.Taints) {
		equal := true
		for i := range next {
			if next[i] != node.Spec.Taints[i] {
				equal = false
				break
			}
		}
		if equal {
			return false
		}
	}
	node.Spec.Taints = next
	return true
}

func (r *NodeActivityReconciler) setCondition(ctx context.Context, activity *availabilityv1alpha1.NodeActivity, status metav1.ConditionStatus, reason, message string, taint *corev1.Taint) error {
	before := activity.DeepCopy()
	meta.SetStatusCondition(&activity.Status.Conditions, metav1.Condition{
		Type:               TaintAppliedCondition,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: activity.Generation,
		LastTransitionTime: metav1.NewTime(r.clock().Now()),
	})
	if taint == nil {
		activity.Status.ManagedTaint = nil
	} else {
		activity.Status.ManagedTaint = &availabilityv1alpha1.ManagedTaintStatus{Key: taint.Key, Value: taint.Value, Effect: string(taint.Effect)}
	}
	if reflect.DeepEqual(before.Status, activity.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, activity, client.MergeFrom(before)); err != nil {
		return err
	}
	return nil
}
