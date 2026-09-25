package controller

import (
	"strings"
	"sync"

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

	metricsMu    sync.Mutex
	lastActivity = make(map[string]activityMetricLabels)
	lastTaint    = make(map[string]taintMetricLabels)
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
	metrics.Registry.MustRegister(activityMetric, taintMetric)
}

func observeActivityAndTaint(nodeName string, state availabilityv1alpha1.State, activity availabilityv1alpha1.Activity, desired *corev1.Taint) {
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
}

func resetMetricsForTesting() {
	metricsMu.Lock()
	defer metricsMu.Unlock()

	activityMetric.Reset()
	taintMetric.Reset()
	lastActivity = make(map[string]activityMetricLabels)
	lastTaint = make(map[string]taintMetricLabels)
}
