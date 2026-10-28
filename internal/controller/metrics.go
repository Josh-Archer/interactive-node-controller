package controller

import (
	"strings"
	"sync"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	activityMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "interactive_node_controller_activity",
		Help: "Current reported activity state of the enrolled node.",
	}, []string{"node", "state", "activity"})

	taintMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "interactive_node_controller_taint",
		Help: "Managed taint currently applied to the enrolled node.",
	}, []string{"node", "key", "value", "effect"})

	schedulableMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "interactive_node_controller_schedulable",
		Help: admissionSignalHelp,
	}, []string{"node"})

	admissionMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "interactive_node_controller_admission",
		Help: "Effective controller admission reason for workloads that select " + schedulableLabelPhrase + ". 1 on the active bounded reason. " + admissionDisclaimer,
	}, []string{"node", "reason"})

	heartbeatAgeMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "interactive_node_controller_heartbeat_age_seconds",
		Help: "Whole seconds since the heartbeat that produced the node policy. Zero when that timestamp is inside the future clock-skew tolerance. Negative when it is further ahead than heartbeat-skew. Absent when the timestamp is missing. status.admission.heartbeatAgeSeconds omits a future timestamp because the API schema rejects a negative age.",
	}, []string{"node"})

	heartbeatValidMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "interactive_node_controller_heartbeat_valid",
		Help: "1 when the heartbeat that produced the node policy is set, younger than stale-after, and not further ahead than heartbeat-skew.",
	}, []string{"node"})

	burstTaintMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "interactive_node_controller_burst_taint",
		Help: "1 when the configured burst NoSchedule taint is present on the Node, 0 when the key is configured and the taint is absent. No series when burst tainting is not configured.",
	}, []string{"node"})

	nextTransitionMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "interactive_node_controller_next_transition_timestamp_seconds",
		Help: "Unix timestamp of the next heartbeat, hold, or scheduled-window boundary that can change managed policy. Window start time remains inc_next_window_start_timestamp_seconds.",
	}, []string{"node"})

	evictionOutcomeMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "interactive_node_controller_eviction_outcome",
		Help: "Current eviction condition outcome. 1 on the active bounded outcome.",
	}, []string{"node", "outcome"})

	evictionBlockedMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "interactive_node_controller_eviction_blocked",
		Help: "1 while eviction is blocked. Reason is pdb-blocked, api-error, eviction-conflict, pod-gone, or retry-backoff.",
	}, []string{"node", "reason"})

	configInvalidMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "interactive_node_controller_config_invalid",
		Help: "1 when the enrollment schedule cannot be evaluated. Reason is schedule.",
	}, []string{"node", "reason"})

	windowActiveMetric = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "inc_window_active", Help: "Whether an availability window or its lead-time taint is active."}, []string{"node"})
	nextWindowMetric   = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "inc_next_window_start_timestamp_seconds", Help: "Unix timestamp of the next scheduled availability window."}, []string{"node"})

	metricsMu           sync.Mutex
	lastActivity        = make(map[string]activityMetricLabels)
	lastTaint           = make(map[string]taintMetricLabels)
	lastAdmissionReason = make(map[string]string)
	lastEvictionOutcome = make(map[string]string)
	lastEvictionBlocker = make(map[string]string)
)

const (
	schedulableLabelPhrase = "availability.interactive-node.io/schedulable"
	admissionDisclaimer    = "This does not mean the kube-scheduler can place an arbitrary workload on the Node."
	admissionSignalHelp    = "1 when this controller stands down its managed taint and sets " + schedulableLabelPhrase + " for workloads that select that label. " + admissionDisclaimer
	configInvalidReason    = "schedule"
)

type activityMetricLabels struct {
	state    string
	activity string
}

type taintMetricLabels struct {
	key    string
	value  string
	effect string
}

func init() {
	metrics.Registry.MustRegister(
		activityMetric,
		taintMetric,
		schedulableMetric,
		admissionMetric,
		heartbeatAgeMetric,
		heartbeatValidMetric,
		burstTaintMetric,
		nextTransitionMetric,
		evictionOutcomeMetric,
		evictionBlockedMetric,
		configInvalidMetric,
		windowActiveMetric,
		nextWindowMetric,
	)
}

func controllerAdmitted(fresh bool, state availabilityv1alpha1.State, activity availabilityv1alpha1.Activity, desired *corev1.Taint) bool {
	return fresh && state == availabilityv1alpha1.StateIdle && activity == availabilityv1alpha1.ActivityIdle && desired == nil
}

func observeWindowMetrics(nodeName string, active bool, next time.Time) {
	if strings.TrimSpace(nodeName) == "" {
		return
	}
	windowActiveMetric.WithLabelValues(nodeName).Set(boolGauge(active))
	if next.IsZero() {
		nextWindowMetric.DeleteLabelValues(nodeName)
	} else {
		nextWindowMetric.WithLabelValues(nodeName).Set(float64(next.Unix()))
	}
}

func boolGauge(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func observeActivityAndTaint(nodeName string, state availabilityv1alpha1.State, activity availabilityv1alpha1.Activity, desired *corev1.Taint, fresh bool) {
	nodeName = strings.TrimSpace(nodeName)
	if nodeName == "" {
		return
	}
	stateStr := string(state)
	if stateStr == "" {
		stateStr = string(availabilityv1alpha1.StateUnknown)
	}
	activityStr := string(activity)
	if activityStr == "" {
		activityStr = string(availabilityv1alpha1.ActivityUnknown)
	}

	metricsMu.Lock()
	defer metricsMu.Unlock()

	oldActivity, hadActivity := lastActivity[nodeName]
	if hadActivity && (oldActivity.state != stateStr || oldActivity.activity != activityStr) {
		activityMetric.DeleteLabelValues(nodeName, oldActivity.state, oldActivity.activity)
	}
	activityMetric.WithLabelValues(nodeName, stateStr, activityStr).Set(1)
	lastActivity[nodeName] = activityMetricLabels{state: stateStr, activity: activityStr}

	oldTaint, hadTaint := lastTaint[nodeName]
	if desired == nil {
		if hadTaint {
			taintMetric.DeleteLabelValues(nodeName, oldTaint.key, oldTaint.value, oldTaint.effect)
			delete(lastTaint, nodeName)
		}
	} else {
		desiredEffect := string(desired.Effect)
		if hadTaint && (oldTaint.key != desired.Key || oldTaint.value != desired.Value || oldTaint.effect != desiredEffect) {
			taintMetric.DeleteLabelValues(nodeName, oldTaint.key, oldTaint.value, oldTaint.effect)
		}
		taintMetric.WithLabelValues(nodeName, desired.Key, desired.Value, desiredEffect).Set(1)
		lastTaint[nodeName] = taintMetricLabels{key: desired.Key, value: desired.Value, effect: desiredEffect}
	}

	if controllerAdmitted(fresh, state, activity, desired) {
		schedulableMetric.WithLabelValues(nodeName).Set(1)
	} else {
		schedulableMetric.WithLabelValues(nodeName).Set(0)
	}
}

func observeAdmission(nodeName string, heartbeatAt, now time.Time, staleAfter, skew time.Duration, reason string, burstConfigured, burstApplied bool, nextTransition time.Time) {
	nodeName = strings.TrimSpace(nodeName)
	if nodeName == "" || reason == "" {
		return
	}
	verdict := heartbeatFreshness(heartbeatAt, now, staleAfter, skew)
	metricsMu.Lock()
	defer metricsMu.Unlock()

	if old, ok := lastAdmissionReason[nodeName]; ok && old != reason {
		admissionMetric.DeleteLabelValues(nodeName, old)
	}
	admissionMetric.WithLabelValues(nodeName, reason).Set(1)
	lastAdmissionReason[nodeName] = reason

	heartbeatValidMetric.WithLabelValues(nodeName).Set(boolGauge(verdict.Fresh))
	switch {
	case verdict.Missing:
		heartbeatAgeMetric.DeleteLabelValues(nodeName)
	case verdict.Future:
		heartbeatAgeMetric.WithLabelValues(nodeName).Set(float64(futureAgeSeconds(verdict.Lead)))
	default:
		heartbeatAgeMetric.WithLabelValues(nodeName).Set(float64(*verdict.AgeSeconds))
	}
	if burstConfigured {
		burstTaintMetric.WithLabelValues(nodeName).Set(boolGauge(burstApplied))
	} else {
		burstTaintMetric.DeleteLabelValues(nodeName)
	}
	if nextTransition.IsZero() {
		nextTransitionMetric.DeleteLabelValues(nodeName)
	} else {
		nextTransitionMetric.WithLabelValues(nodeName).Set(float64(nextTransition.Unix()))
	}
}

func observeEvictionStatus(nodeName, outcome, blocker string) {
	nodeName = strings.TrimSpace(nodeName)
	outcome = strings.TrimSpace(outcome)
	if nodeName == "" || outcome == "" {
		return
	}
	metricsMu.Lock()
	defer metricsMu.Unlock()

	if old, ok := lastEvictionOutcome[nodeName]; ok && old != outcome {
		evictionOutcomeMetric.DeleteLabelValues(nodeName, old)
	}
	evictionOutcomeMetric.WithLabelValues(nodeName, outcome).Set(1)
	lastEvictionOutcome[nodeName] = outcome

	if old, ok := lastEvictionBlocker[nodeName]; ok && (outcome != evictionStateBlocked || old != blocker) {
		evictionBlockedMetric.DeleteLabelValues(nodeName, old)
		delete(lastEvictionBlocker, nodeName)
	}
	if outcome == evictionStateBlocked {
		blocker = boundedBlockerReason(blocker)
		evictionBlockedMetric.WithLabelValues(nodeName, blocker).Set(1)
		lastEvictionBlocker[nodeName] = blocker
	}
}

func observeConfigInvalid(nodeName string, invalid bool) {
	nodeName = strings.TrimSpace(nodeName)
	if nodeName == "" {
		return
	}
	if invalid {
		configInvalidMetric.WithLabelValues(nodeName, configInvalidReason).Set(1)
		return
	}
	configInvalidMetric.DeleteLabelValues(nodeName, configInvalidReason)
}

func clearNodeMetrics(nodeName string) {
	nodeName = strings.TrimSpace(nodeName)
	if nodeName == "" {
		return
	}
	metricsMu.Lock()
	defer metricsMu.Unlock()

	if oldActivity, hadActivity := lastActivity[nodeName]; hadActivity {
		activityMetric.DeleteLabelValues(nodeName, oldActivity.state, oldActivity.activity)
		delete(lastActivity, nodeName)
	}
	if oldTaint, hadTaint := lastTaint[nodeName]; hadTaint {
		taintMetric.DeleteLabelValues(nodeName, oldTaint.key, oldTaint.value, oldTaint.effect)
		delete(lastTaint, nodeName)
	}
	schedulableMetric.DeleteLabelValues(nodeName)
	windowActiveMetric.DeleteLabelValues(nodeName)
	nextWindowMetric.DeleteLabelValues(nodeName)
	if reason, ok := lastAdmissionReason[nodeName]; ok {
		admissionMetric.DeleteLabelValues(nodeName, reason)
		delete(lastAdmissionReason, nodeName)
	}
	heartbeatValidMetric.DeleteLabelValues(nodeName)
	heartbeatAgeMetric.DeleteLabelValues(nodeName)
	burstTaintMetric.DeleteLabelValues(nodeName)
	nextTransitionMetric.DeleteLabelValues(nodeName)
	if outcome, ok := lastEvictionOutcome[nodeName]; ok {
		evictionOutcomeMetric.DeleteLabelValues(nodeName, outcome)
		delete(lastEvictionOutcome, nodeName)
	}
	if blocker, ok := lastEvictionBlocker[nodeName]; ok {
		evictionBlockedMetric.DeleteLabelValues(nodeName, blocker)
		delete(lastEvictionBlocker, nodeName)
	}
	configInvalidMetric.DeleteLabelValues(nodeName, configInvalidReason)
}

func resetMetricsForTesting() {
	metricsMu.Lock()
	defer metricsMu.Unlock()

	activityMetric.Reset()
	taintMetric.Reset()
	schedulableMetric.Reset()
	admissionMetric.Reset()
	heartbeatAgeMetric.Reset()
	heartbeatValidMetric.Reset()
	burstTaintMetric.Reset()
	nextTransitionMetric.Reset()
	evictionOutcomeMetric.Reset()
	evictionBlockedMetric.Reset()
	configInvalidMetric.Reset()
	windowActiveMetric.Reset()
	nextWindowMetric.Reset()
	lastActivity = make(map[string]activityMetricLabels)
	lastTaint = make(map[string]taintMetricLabels)
	lastAdmissionReason = make(map[string]string)
	lastEvictionOutcome = make(map[string]string)
	lastEvictionBlocker = make(map[string]string)
}
