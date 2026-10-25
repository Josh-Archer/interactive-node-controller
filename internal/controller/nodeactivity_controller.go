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
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

const (
	TaintAppliedCondition = "TaintApplied"
	// InvalidScheduleCondition is True when spec.leadTime or spec.windows cannot
	// be evaluated. The message names each bad field. Heartbeat and live-activity
	// protection still reconcile.
	InvalidScheduleCondition = "InvalidSchedule"
	NodeActivityFinalizer    = "availability.interactive-node.io/cleanup"
	SchedulableNodeLabel     = "availability.interactive-node.io/schedulable"
	HoldAnnotation           = "availability.interactive-node.io/hold"
	holdStartAnnotation      = "availability.interactive-node.io/hold-start"
)

// TaintPolicy is deliberately small: one controller instance owns one primary taint
// key and optionally a secondary burst taint key.
type TaintPolicy struct {
	Key              string
	InteractiveValue string
	ActiveValue      string
	FailClosedValue  string
	BurstTaintKey    string
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
	if p.BurstTaintKey == "" {
		return nil
	}
	if strings.ContainsAny(p.BurstTaintKey, " \t\r\n") {
		return fmt.Errorf("burst taint key %q cannot contain whitespace", p.BurstTaintKey)
	}
	if msgs := validation.IsQualifiedName(p.BurstTaintKey); len(msgs) > 0 {
		return fmt.Errorf("burst taint key %q is invalid: %s", p.BurstTaintKey, strings.Join(msgs, "; "))
	}
	if p.BurstTaintKey == p.Key {
		return fmt.Errorf("burst taint key %q collides with the primary taint key", p.BurstTaintKey)
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
	retries  evictionRetries
	// budgets are process-local. A restart or leader switch starts them full.
	budgets evictionBudgets
	// controllerName overrides the generated controller name when several
	// managers are started in one process.
	controllerName string
}

func (r *NodeActivityReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	activity := &availabilityv1alpha1.NodeActivity{}
	if err := r.Get(ctx, request.NamespacedName, activity); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if err := r.Policy.Validate(); err != nil {
		return ctrl.Result{}, err
	}

	if !activity.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(activity, NodeActivityFinalizer) {
			if err := r.finalize(ctx, activity); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(activity, NodeActivityFinalizer)
			if err := r.Update(ctx, activity); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	scheduleErr := validateSchedule(activity.Spec)
	if strings.TrimSpace(activity.Spec.NodeName) == "" {
		if err := r.setCondition(ctx, activity, metav1.ConditionFalse, "InvalidEnrollment", "spec.nodeName is required", nil); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.setScheduleCondition(ctx, activity, scheduleErr); err != nil {
			return ctrl.Result{}, err
		}
		valid, age := heartbeatObservation(activity.Status.HeartbeatAt, r.clock().Now(), r.Policy.StaleAfter)
		return ctrl.Result{}, r.setAdmission(ctx, activity, &availabilityv1alpha1.AdmissionStatus{
			Admitted:            false,
			Reason:              admissionInvalidEnrollment,
			Message:             "spec.nodeName is required",
			HeartbeatValid:      valid,
			HeartbeatAgeSeconds: age,
		})
	}

	if !controllerutil.ContainsFinalizer(activity, NodeActivityFinalizer) {
		controllerutil.AddFinalizer(activity, NodeActivityFinalizer)
		if err := r.Update(ctx, activity); err != nil {
			return ctrl.Result{}, err
		}
	}

	now := r.clock().Now()
	if err := r.reconcileHold(ctx, activity, now); err != nil {
		return ctrl.Result{}, err
	}
	protection, err := r.protectionForNode(ctx, activity.Spec.NodeName, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	desired, reason, requeueAfter := protection.taint, protection.reason, protection.requeueAfter
	admitted := r.policyAdmitted(protection, now)
	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: activity.Spec.NodeName}, node); err != nil {
		if apierrors.IsNotFound(err) {
			clearNodeMetrics(activity.Spec.NodeName)
			if err := r.setCondition(ctx, activity, metav1.ConditionFalse, "NodeNotFound", "enrolled node does not exist", desired); err != nil {
				return ctrl.Result{}, err
			}
			if err := r.setScheduleCondition(ctx, activity, scheduleErr); err != nil {
				return ctrl.Result{}, err
			}
			if err := r.setAdmission(ctx, activity, r.admissionStatus(protection, false, false, admissionNodeNotFound)); err != nil {
				return ctrl.Result{}, err
			}
			if err := r.setEvictionCondition(ctx, activity, r.evictionWithoutPods(true)); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}
		return ctrl.Result{}, err
	}

	changedLabel := reconcileSchedulableLabel(node, admitted)
	changedPrimary := reconcileOwnedTaint(node, r.Policy.Key, desired)
	var changedBurst bool
	if r.Policy.BurstTaintKey != "" {
		var desiredBurst *corev1.Taint
		if !admitted {
			desiredBurst = &corev1.Taint{
				Key:    r.Policy.BurstTaintKey,
				Value:  r.Policy.FailClosedValue,
				Effect: corev1.TaintEffectNoSchedule,
			}
		}
		changedBurst = reconcileOwnedTaint(node, r.Policy.BurstTaintKey, desiredBurst)
	}

	if changedLabel || changedPrimary || changedBurst {
		if err := r.Update(ctx, node); err != nil {
			return ctrl.Result{}, err
		}
	}
	conditionStatus, conditionReason := metav1.ConditionTrue, "TaintReconciled"
	if protection.scheduled {
		conditionReason = "ScheduledWindow"
	}
	if protection.count > 1 {
		conditionStatus, conditionReason = metav1.ConditionFalse, "DuplicateEnrollment"
	}
	if err := r.setCondition(ctx, activity, conditionStatus, conditionReason, reason, desired); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.setScheduleCondition(ctx, activity, scheduleErr); err != nil {
		return ctrl.Result{}, err
	}
	burstApplied := r.Policy.BurstTaintKey != "" && hasTaint(node, r.Policy.BurstTaintKey, r.Policy.FailClosedValue, corev1.TaintEffectNoSchedule)
	if err := r.setAdmission(ctx, activity, r.admissionStatus(protection, admitted, burstApplied, "")); err != nil {
		return ctrl.Result{}, err
	}
	r.observeNodeSignals(activity.Spec.NodeName, protection, burstApplied, scheduleErr != nil)
	evictionSummary := r.evictionWithoutPods(false)
	if protection.count == 1 {
		evictionSummary, err = r.reconcileEvictions(ctx, activity, node, desired)
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.setEvictionCondition(ctx, activity, evictionSummary); err != nil {
		return ctrl.Result{}, err
	}
	observeEvictionStatus(activity.Spec.NodeName, evictionSummary.outcome, evictionSummary.blockerReason)
	result := ctrl.Result{RequeueAfter: requeueAfter}
	if evictionSummary.requeueAfter > 0 && (result.RequeueAfter <= 0 || evictionSummary.requeueAfter < result.RequeueAfter) {
		result.RequeueAfter = evictionSummary.requeueAfter
	}
	return result, nil
}

func (r *NodeActivityReconciler) policyAdmitted(protection enrollmentProtection, now time.Time) bool {
	if protection.source == nil {
		return false
	}
	fresh := heartbeatFresh(protection.source.Status.HeartbeatAt, now, r.Policy.StaleAfter)
	return controllerAdmitted(fresh, protection.source.Status.State, protection.source.Status.Activity, protection.taint)
}

func (r *NodeActivityReconciler) evictionWithoutPods(nodeMissing bool) evictionSummary {
	if !r.Eviction.normalized().Enabled && !r.Eviction.normalized().Audit {
		return evictionSummary{outcome: evictionStateDisabled, message: "eviction is disabled"}
	}
	if nodeMissing {
		return evictionSummary{outcome: evictionStateInactive, message: "eviction policy is inactive because the enrolled node does not exist"}
	}
	return evictionSummary{outcome: evictionStateInactive, message: "eviction suppressed while enrollment is ambiguous"}
}

func (r *NodeActivityReconciler) admissionStatus(protection enrollmentProtection, admitted, burstApplied bool, reasonOverride string) *availabilityv1alpha1.AdmissionStatus {
	now := r.clock().Now()
	heartbeatAt := time.Time{}
	if protection.source != nil {
		heartbeatAt = protection.source.Status.HeartbeatAt
	}
	valid, age := heartbeatObservation(heartbeatAt, now, r.Policy.StaleAfter)
	reason := protection.admissionReason
	message := protection.reason
	if reasonOverride == admissionNodeNotFound {
		reason = admissionNodeNotFound
		message = "enrolled node does not exist"
		admitted = false
		burstApplied = false
	}
	if reason == "" {
		reason = admissionUnsupportedState
	}
	status := &availabilityv1alpha1.AdmissionStatus{
		Admitted:            admitted,
		Reason:              reason,
		Message:             message,
		HeartbeatValid:      valid,
		HeartbeatAgeSeconds: age,
		BurstTaint: availabilityv1alpha1.BurstTaintStatus{
			Configured: r.Policy.BurstTaintKey != "",
			Desired:    r.Policy.BurstTaintKey != "" && !r.policyAdmitted(protection, now),
			Applied:    burstApplied,
		},
	}
	if !protection.nextTransition.IsZero() {
		// UTC drops the monotonic clock and the window location so later
		// reconciles compare equal after an API round trip.
		next := metav1.NewTime(protection.nextTransition.UTC())
		status.NextTransition = &next
		status.NextTransitionReason = protection.transitionReason
	}
	return status
}

func (r *NodeActivityReconciler) setAdmission(ctx context.Context, activity *availabilityv1alpha1.NodeActivity, admission *availabilityv1alpha1.AdmissionStatus) error {
	before := activity.DeepCopy()
	activity.Status.Admission = admission
	if reflect.DeepEqual(before.Status, activity.Status) {
		return nil
	}
	return r.Status().Patch(ctx, activity, client.MergeFrom(before))
}

func (r *NodeActivityReconciler) observeNodeSignals(nodeName string, protection enrollmentProtection, burstApplied, scheduleInvalid bool) {
	if strings.TrimSpace(nodeName) == "" || protection.source == nil {
		clearNodeMetrics(nodeName)
		return
	}
	now := r.clock().Now()
	fresh := heartbeatFresh(protection.source.Status.HeartbeatAt, now, r.Policy.StaleAfter)
	reason := protection.admissionReason
	if reason == "" {
		reason = admissionUnsupportedState
	}
	observeWindowMetrics(nodeName, protection.scheduled, protection.nextWindow)
	observeActivityAndTaint(nodeName, protection.source.Status.State, protection.source.Status.Activity, protection.taint, fresh)
	observeAdmission(nodeName, protection.source.Status.HeartbeatAt, now, r.Policy.StaleAfter, reason, r.Policy.BurstTaintKey != "", burstApplied, protection.nextTransition)
	observeConfigInvalid(nodeName, scheduleInvalid)
}

func (r *NodeActivityReconciler) setEvictionCondition(ctx context.Context, activity *availabilityv1alpha1.NodeActivity, summary evictionSummary) error {
	before := activity.DeepCopy()
	meta.SetStatusCondition(&activity.Status.Conditions, summary.condition(r.clock().Now(), activity.Generation))
	if reflect.DeepEqual(before.Status, activity.Status) {
		return nil
	}
	return r.Status().Patch(ctx, activity, client.MergeFrom(before))
}

func (r *NodeActivityReconciler) finalize(ctx context.Context, activity *availabilityv1alpha1.NodeActivity) error {
	nodeName := strings.TrimSpace(activity.Spec.NodeName)
	if nodeName == "" {
		return nil
	}
	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		if apierrors.IsNotFound(err) {
			clearNodeMetrics(nodeName)
			return nil
		}
		return err
	}
	protection, err := r.protectionForNode(ctx, nodeName, r.clock().Now())
	if err != nil {
		return err
	}
	changedLabel := reconcileSchedulableLabel(node, false)
	changedPrimary := reconcileOwnedTaint(node, r.Policy.Key, protection.taint)
	var changedBurst bool
	if r.Policy.BurstTaintKey != "" {
		var desiredBurst *corev1.Taint
		if protection.source != nil {
			fresh := !protection.source.Status.HeartbeatAt.IsZero() && r.clock().Now().Sub(protection.source.Status.HeartbeatAt) < r.Policy.StaleAfter
			if !(fresh && protection.source.Status.State == availabilityv1alpha1.StateIdle && protection.source.Status.Activity == availabilityv1alpha1.ActivityIdle && protection.taint == nil) {
				desiredBurst = &corev1.Taint{
					Key:    r.Policy.BurstTaintKey,
					Value:  r.Policy.FailClosedValue,
					Effect: corev1.TaintEffectNoSchedule,
				}
			}
		}
		changedBurst = reconcileOwnedTaint(node, r.Policy.BurstTaintKey, desiredBurst)
	}
	if changedLabel || changedPrimary || changedBurst {
		if err := r.Update(ctx, node); err != nil {
			return err
		}
	}
	if protection.source == nil {
		clearNodeMetrics(nodeName)
		return nil
	}
	burstApplied := r.Policy.BurstTaintKey != "" && hasTaint(node, r.Policy.BurstTaintKey, r.Policy.FailClosedValue, corev1.TaintEffectNoSchedule)
	r.observeNodeSignals(nodeName, protection, burstApplied, false)
	if !r.Eviction.normalized().Enabled && !r.Eviction.normalized().Audit {
		observeEvictionStatus(nodeName, evictionStateDisabled, "")
	} else if protection.count != 1 {
		observeEvictionStatus(nodeName, evictionStateInactive, "")
	}
	return nil
}

const (
	PodNodeNameKey          = "spec.nodeName"
	NodeActivityNodeNameKey = "spec.nodeName"
)

// IndexPodNodeName extracts the pod's assigned node name for field indexing.
func IndexPodNodeName(rawObj client.Object) []string {
	pod, ok := rawObj.(*corev1.Pod)
	if !ok || pod.Spec.NodeName == "" {
		return nil
	}
	return []string{pod.Spec.NodeName}
}

// SetupPodNodeNameIndexer registers the field indexer for pod spec.nodeName on the given FieldIndexer.
func SetupPodNodeNameIndexer(ctx context.Context, indexer client.FieldIndexer) error {
	return indexer.IndexField(ctx, &corev1.Pod{}, PodNodeNameKey, IndexPodNodeName)
}

// IndexNodeActivityNodeName extracts the enrolled node name for field indexing.
// Empty names are omitted so they do not share an index key with a blank query.
func IndexNodeActivityNodeName(rawObj client.Object) []string {
	activity, ok := rawObj.(*availabilityv1alpha1.NodeActivity)
	if !ok || activity.Spec.NodeName == "" {
		return nil
	}
	return []string{activity.Spec.NodeName}
}

// SetupNodeActivityNodeNameIndexer registers the field indexer for NodeActivity spec.nodeName.
func SetupNodeActivityNodeNameIndexer(ctx context.Context, indexer client.FieldIndexer) error {
	return indexer.IndexField(ctx, &availabilityv1alpha1.NodeActivity{}, NodeActivityNodeNameKey, IndexNodeActivityNodeName)
}

func (r *NodeActivityReconciler) SetupWithManager(manager ctrl.Manager) error {
	if err := SetupPodNodeNameIndexer(context.Background(), manager.GetFieldIndexer()); err != nil {
		return err
	}
	if err := SetupNodeActivityNodeNameIndexer(context.Background(), manager.GetFieldIndexer()); err != nil {
		return err
	}
	managed := ctrl.NewControllerManagedBy(manager)
	if r.controllerName != "" {
		managed = managed.Named(r.controllerName)
	}
	return managed.
		For(&availabilityv1alpha1.NodeActivity{}).
		Watches(&availabilityv1alpha1.NodeActivity{}, handler.EnqueueRequestsFromMapFunc(r.requestsForNode)).
		// Node watch: an out-of-band taint or schedulable-label edit enqueues the
		// enrollments for that Node instead of waiting for the next status write.
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.requestsForEnrolledNode), builder.WithPredicates(r.nodeOwnedFieldsChanged())).
		Complete(r)
}

func (r *NodeActivityReconciler) clock() clock.Clock {
	if r.Clock != nil {
		return r.Clock
	}
	return clock.RealClock{}
}

func (r *NodeActivityReconciler) desiredTaint(activity *availabilityv1alpha1.NodeActivity, now time.Time) (*corev1.Taint, string, time.Duration, bool, time.Time, error) {
	decision, err := r.decidePolicy(activity, now)
	if err != nil {
		return nil, "", 0, false, time.Time{}, err
	}
	return decision.taint, decision.message, decision.requeueAfter, decision.scheduled, decision.nextWindow, nil
}

const (
	admissionFreshIdle            = "fresh-idle"
	admissionActiveGame           = "active-game"
	admissionActiveInteractive    = "active-interactive"
	admissionHold                 = "availability-hold"
	admissionScheduledGame        = "scheduled-game"
	admissionScheduledInteractive = "scheduled-interactive"
	admissionScheduledIdle        = "scheduled-idle"
	admissionStaleHeartbeat       = "stale-heartbeat"
	admissionMissingHeartbeat     = "missing-heartbeat"
	admissionInvalidSchedule      = "invalid-schedule"
	admissionUnknownState         = "unknown-state"
	admissionStaleState           = "stale-state"
	admissionUnsupportedActivity  = "unsupported-activity"
	admissionUnsupportedState     = "unsupported-state"
	admissionNodeNotFound         = "node-not-found"
	admissionInvalidEnrollment    = "invalid-enrollment"

	transitionHeartbeat = "heartbeat"
	transitionHold      = "hold"
	transitionWindow    = "window"
)

type policyDecision struct {
	taint            *corev1.Taint
	message          string
	admissionReason  string
	requeueAfter     time.Duration
	scheduled        bool
	nextWindow       time.Time
	nextTransition   time.Time
	transitionReason string
}

type transitionCandidate struct {
	at   time.Time
	kind string
}

func earliestTransition(now time.Time, candidates ...transitionCandidate) (time.Time, string) {
	var best time.Time
	kind := ""
	for _, candidate := range candidates {
		if candidate.at.IsZero() || !candidate.at.After(now) || candidate.kind == "" {
			continue
		}
		if best.IsZero() || candidate.at.Before(best) {
			best = candidate.at
			kind = candidate.kind
		}
	}
	return best, kind
}

func (r *NodeActivityReconciler) decidePolicy(activity *availabilityv1alpha1.NodeActivity, now time.Time) (policyDecision, error) {
	window, nextWindow, windowWait, windowErr := evaluateWindows(activity.Spec, now)
	if windowErr != nil {
		// Invalid windows are surfaced as InvalidSchedule. Ignore them for
		// scheduling, but do not skip live activity or heartbeat protection.
		window = nil
		nextWindow = time.Time{}
		windowWait = 0
	}
	decision := policyDecision{nextWindow: nextWindow}
	var windowBoundary transitionCandidate
	if windowWait > 0 {
		windowBoundary = transitionCandidate{at: now.Add(windowWait), kind: transitionWindow}
	}
	if !heartbeatFresh(activity.Status.HeartbeatAt, now, r.Policy.StaleAfter) {
		taint, message, delay := r.failClosed("host heartbeat is missing or stale")
		decision.taint = taint
		decision.message = message
		decision.requeueAfter = minPositive(delay, windowWait)
		decision.admissionReason = admissionStaleHeartbeat
		if activity.Status.HeartbeatAt.IsZero() {
			decision.admissionReason = admissionMissingHeartbeat
		}
		return decision, nil
	}
	heartbeatDeadline := activity.Status.HeartbeatAt.Add(r.Policy.StaleAfter)
	remaining := heartbeatDeadline.Sub(now)
	if remaining <= 0 {
		remaining = time.Second
	}
	heartbeat := transitionCandidate{at: heartbeatDeadline, kind: transitionHeartbeat}
	switch activity.Status.State {
	case availabilityv1alpha1.StateIdle:
		if holdUntil := holdExpiry(activity); holdUntil.After(now) {
			decision.taint = &corev1.Taint{Key: r.Policy.Key, Value: r.Policy.InteractiveValue, Effect: corev1.TaintEffectPreferNoSchedule}
			decision.message = "time-boxed availability hold"
			decision.admissionReason = admissionHold
			decision.requeueAfter = minPositive(remaining, holdUntil.Sub(now))
			decision.nextTransition, decision.transitionReason = earliestTransition(now, heartbeat, transitionCandidate{at: holdUntil, kind: transitionHold})
			return decision, nil
		}
		if window != nil {
			decision.scheduled = true
			decision.requeueAfter = minPositive(remaining, windowWait)
			decision.nextTransition, decision.transitionReason = earliestTransition(now, heartbeat, windowBoundary)
			switch window.state {
			case "game":
				decision.taint = &corev1.Taint{Key: r.Policy.Key, Value: r.Policy.ActiveValue, Effect: corev1.TaintEffectNoSchedule}
				decision.message = "scheduled game window"
				decision.admissionReason = admissionScheduledGame
			case "interactive":
				decision.taint = &corev1.Taint{Key: r.Policy.Key, Value: r.Policy.InteractiveValue, Effect: corev1.TaintEffectPreferNoSchedule}
				decision.message = "scheduled interactive window"
				decision.admissionReason = admissionScheduledInteractive
			default:
				decision.message = "scheduled idle-allowed window"
				decision.admissionReason = admissionScheduledIdle
			}
			return decision, nil
		}
		if windowErr != nil {
			// A fresh idle heartbeat must not stay admitted when the schedule
			// cannot prove that no protected window is open.
			taint, message, delay := r.failClosed("availability schedule is invalid")
			decision.taint = taint
			decision.message = message
			decision.admissionReason = admissionInvalidSchedule
			decision.requeueAfter = minPositive(delay, windowWait)
			if taint == nil {
				decision.nextTransition, decision.transitionReason = earliestTransition(now, heartbeat)
			}
			return decision, nil
		}
		decision.message = "host is idle; managed taint removed"
		decision.admissionReason = admissionFreshIdle
		decision.requeueAfter = minPositive(remaining, windowWait)
		decision.nextTransition, decision.transitionReason = earliestTransition(now, heartbeat, windowBoundary)
		return decision, nil
	case availabilityv1alpha1.StateActive:
		if activity.Status.Activity == availabilityv1alpha1.ActivityGame {
			decision.taint = &corev1.Taint{Key: r.Policy.Key, Value: r.Policy.ActiveValue, Effect: corev1.TaintEffectNoSchedule}
			decision.message = "host is running an active game"
			decision.admissionReason = admissionActiveGame
			decision.requeueAfter = minPositive(remaining, windowWait)
			// A live game keeps its taint across window boundaries. The heartbeat
			// deadline is the next clock-driven policy change.
			decision.nextTransition, decision.transitionReason = earliestTransition(now, heartbeat)
			return decision, nil
		}
		if holdUntil := holdExpiry(activity); holdUntil.After(now) {
			decision.taint = &corev1.Taint{Key: r.Policy.Key, Value: r.Policy.InteractiveValue, Effect: corev1.TaintEffectPreferNoSchedule}
			decision.message = "time-boxed availability hold"
			decision.admissionReason = admissionHold
			decision.requeueAfter = minPositive(remaining, holdUntil.Sub(now))
			decision.nextTransition, decision.transitionReason = earliestTransition(now, heartbeat, transitionCandidate{at: holdUntil, kind: transitionHold})
			return decision, nil
		}
		if window != nil && (window.state == "game" || window.state == "idle-allowed") {
			decision.scheduled = true
			decision.requeueAfter = minPositive(remaining, windowWait)
			decision.nextTransition, decision.transitionReason = earliestTransition(now, heartbeat, windowBoundary)
			if window.state == "game" {
				decision.taint = &corev1.Taint{Key: r.Policy.Key, Value: r.Policy.ActiveValue, Effect: corev1.TaintEffectNoSchedule}
				decision.message = "scheduled game window"
				decision.admissionReason = admissionScheduledGame
				return decision, nil
			}
			decision.message = "scheduled idle-allowed window"
			decision.admissionReason = admissionScheduledIdle
			return decision, nil
		}
		if activity.Status.Activity == availabilityv1alpha1.ActivityInteractive {
			decision.taint = &corev1.Taint{Key: r.Policy.Key, Value: r.Policy.InteractiveValue, Effect: corev1.TaintEffectPreferNoSchedule}
			decision.message = "host is interactive"
			decision.admissionReason = admissionActiveInteractive
			decision.requeueAfter = minPositive(remaining, windowWait)
			decision.nextTransition, decision.transitionReason = earliestTransition(now, heartbeat, windowBoundary)
			return decision, nil
		}
		taint, message, delay := r.failClosed("active state has an unsupported activity")
		decision.taint = taint
		decision.message = message
		decision.admissionReason = admissionUnsupportedActivity
		decision.requeueAfter = minPositive(delay, windowWait)
		return decision, nil
	case availabilityv1alpha1.StateUnknown, availabilityv1alpha1.StateStale:
		taint, message, delay := r.failClosed("host state is unknown or stale")
		decision.taint = taint
		decision.message = message
		decision.requeueAfter = minPositive(delay, windowWait)
		decision.admissionReason = admissionUnknownState
		if activity.Status.State == availabilityv1alpha1.StateStale {
			decision.admissionReason = admissionStaleState
		}
		return decision, nil
	default:
		taint, message, delay := r.failClosed("host reported an unsupported state")
		decision.taint = taint
		decision.message = message
		decision.admissionReason = admissionUnsupportedState
		decision.requeueAfter = minPositive(delay, windowWait)
		return decision, nil
	}
}

func minPositive(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if b <= 0 || a < b {
		return a
	}
	return b
}

func holdExpiry(activity *availabilityv1alpha1.NodeActivity) time.Time {
	if activity.Annotations == nil || activity.Annotations[HoldAnnotation] == "" {
		return time.Time{}
	}
	duration, err := time.ParseDuration(activity.Annotations[HoldAnnotation])
	if err != nil || duration <= 0 {
		return time.Time{}
	}
	start, err := time.Parse(time.RFC3339Nano, activity.Annotations[holdStartAnnotation])
	if err != nil {
		return time.Time{}
	}
	return start.Add(duration)
}

// reconcileHold anchors duration-based overrides once, then removes both
// annotations when their time box expires.
func (r *NodeActivityReconciler) reconcileHold(ctx context.Context, activity *availabilityv1alpha1.NodeActivity, now time.Time) error {
	value := ""
	if activity.Annotations != nil {
		value = activity.Annotations[HoldAnnotation]
	}
	if value == "" {
		if activity.Annotations == nil || activity.Annotations[holdStartAnnotation] == "" {
			return nil
		}
		before := activity.DeepCopy()
		delete(activity.Annotations, holdStartAnnotation)
		return r.Patch(ctx, activity, client.MergeFrom(before))
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return fmt.Errorf("annotation %s must be a positive duration", HoldAnnotation)
	}
	if activity.Annotations[holdStartAnnotation] == "" {
		before := activity.DeepCopy()
		if activity.Annotations == nil {
			activity.Annotations = make(map[string]string)
		}
		activity.Annotations[holdStartAnnotation] = now.UTC().Format(time.RFC3339Nano)
		return r.Patch(ctx, activity, client.MergeFrom(before))
	}
	if !holdExpiry(activity).After(now) {
		before := activity.DeepCopy()
		delete(activity.Annotations, HoldAnnotation)
		delete(activity.Annotations, holdStartAnnotation)
		return r.Patch(ctx, activity, client.MergeFrom(before))
	}
	return nil
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

func reconcileSchedulableLabel(node *corev1.Node, schedulable bool) bool {
	if schedulable {
		if node.Labels == nil {
			node.Labels = make(map[string]string)
		}
		if node.Labels[SchedulableNodeLabel] != "true" {
			node.Labels[SchedulableNodeLabel] = "true"
			return true
		}
		return false
	}
	if node.Labels != nil {
		if _, exists := node.Labels[SchedulableNodeLabel]; exists {
			delete(node.Labels, SchedulableNodeLabel)
			return true
		}
	}
	return false
}

func (r *NodeActivityReconciler) setScheduleCondition(ctx context.Context, activity *availabilityv1alpha1.NodeActivity, scheduleErr error) error {
	existing := meta.FindStatusCondition(activity.Status.Conditions, InvalidScheduleCondition)
	if scheduleErr == nil && (existing == nil || existing.Status == metav1.ConditionFalse) {
		return nil
	}
	before := activity.DeepCopy()
	condition := metav1.Condition{
		Type:               InvalidScheduleCondition,
		Status:             metav1.ConditionFalse,
		Reason:             "ScheduleValid",
		Message:            "availability schedule is valid",
		ObservedGeneration: activity.Generation,
		LastTransitionTime: metav1.NewTime(r.clock().Now()),
	}
	if scheduleErr != nil {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "InvalidSchedule"
		condition.Message = scheduleErr.Error() + "; scheduled windows were not applied; live activity and heartbeat protection still apply"
	}
	meta.SetStatusCondition(&activity.Status.Conditions, condition)
	if reflect.DeepEqual(before.Status, activity.Status) {
		return nil
	}
	return r.Status().Patch(ctx, activity, client.MergeFrom(before))
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
