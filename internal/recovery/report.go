package recovery

import "time"

// Report is the evidence bundle for one canary run.
// PerformanceClaimed stays false for Kind. Readiness and eviction acceptance
// do not set it.
type Report struct {
	SchemaVersion            string              `json:"schemaVersion"`
	Environment              string              `json:"environment"`
	DetectionScope           string              `json:"detectionScope"`
	Budget                   Budget              `json:"budget"`
	PerformedAt              time.Time           `json:"performedAt"`
	ClusterContext           string              `json:"clusterContext"`
	NodeName                 string              `json:"nodeName"`
	Controller               string              `json:"controller"`
	GPUMeasured              bool                `json:"gpuMeasured"`
	GPUEvidence              string              `json:"gpuEvidence"`
	DesktopVisible           bool                `json:"desktopVisible"`
	DesktopEvidence          string              `json:"desktopEvidence"`
	PerformanceClaimed       bool                `json:"performanceClaimed"`
	UnrelatedTaintPreserved  bool                `json:"unrelatedTaintPreserved"`
	ProtectedNamespaces      []NamespaceSnapshot `json:"protectedNamespacesBefore"`
	ProtectedNamespacesAfter []NamespaceSnapshot `json:"protectedNamespacesAfter"`
	WorkloadCatalog          []WorkloadProfile   `json:"workloadCatalog"`
	Scenarios                []ScenarioResult    `json:"scenarios"`
	LiveRunRemaining         []string            `json:"liveRunRemaining"`
}

// NamespaceSnapshot records pod UIDs in a namespace the canary must not touch.
type NamespaceSnapshot struct {
	Namespace string   `json:"namespace"`
	PodUIDs   []string `json:"podUIDs"`
}

// ScenarioResult is one exercised transition.
type ScenarioResult struct {
	Name                   string                `json:"name"`
	Samples                []PhaseSample         `json:"samples"`
	Workloads              []WorkloadObservation `json:"workloads"`
	Blocked                []BlockedCase         `json:"blocked,omitempty"`
	Notes                  []string              `json:"notes,omitempty"`
	LeadTime               JSONDuration          `json:"leadTime,omitempty"`
	WindowStart            *time.Time            `json:"windowStart,omitempty"`
	PreWindowReleaseProven bool                  `json:"preWindowReleaseProven"`
}

// PhaseSample is one measured phase. SatisfiesRecovery is true only for a
// resource-release sample whose evidence shows the pod is gone and its
// requests are no longer held on the node.
type PhaseSample struct {
	Phase               string       `json:"phase"`
	Applicable          bool         `json:"applicable"`
	Expected            string       `json:"expected"`
	Trigger             string       `json:"trigger"`
	TriggerAt           time.Time    `json:"triggerAt"`
	ObservedAt          *time.Time   `json:"observedAt,omitempty"`
	DurationMillis      *int64       `json:"durationMillis,omitempty"`
	Budget              JSONDuration `json:"budget"`
	WithinBudget        bool         `json:"withinBudget"`
	SatisfiesRecovery   bool         `json:"satisfiesRecovery"`
	AcceptanceGapMillis *int64       `json:"acceptanceGapMillis,omitempty"`
	Evidence            string       `json:"evidence"`
}

// WorkloadObservation is the size and identity of one workload across a scenario.
type WorkloadObservation struct {
	Name                  string `json:"name"`
	Role                  string `json:"role"`
	Disposable            bool   `json:"disposable"`
	OptedIn               bool   `json:"optedIn"`
	Lossy                 bool   `json:"lossy"`
	Pinned                bool   `json:"pinned"`
	CPUMillis             int64  `json:"cpuMillis"`
	MemoryBytes           int64  `json:"memoryBytes"`
	GPU                   int64  `json:"gpu"`
	UIDBefore             string `json:"uidBefore"`
	UIDAfter              string `json:"uidAfter"`
	PhaseAfter            string `json:"phaseAfter"`
	HoldsResourcesAfter   bool   `json:"holdsResourcesAfter"`
	EvictedDuringScenario bool   `json:"evictedDuringScenario"`
	Stage                 string `json:"stage,omitempty"`
}

// BlockedCase records an eviction that the API refused. ResourcesReleased must
// stay false: a PDB block is not recovery and not acceptance.
type BlockedCase struct {
	Workload          string `json:"workload"`
	Reason            string `json:"reason"`
	EvictionAccepted  bool   `json:"evictionAccepted"`
	ResourcesReleased bool   `json:"resourcesReleased"`
	Evidence          string `json:"evidence"`
}
