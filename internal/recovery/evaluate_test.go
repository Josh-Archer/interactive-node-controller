package recovery

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEvaluateAcceptsKindReport(t *testing.T) {
	report := passingKindReport()
	if problems := Evaluate(report); len(problems) != 0 {
		t.Fatalf("problems: %s", strings.Join(problems, "\n"))
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if problems := Evaluate(decoded); len(problems) != 0 {
		t.Fatalf("round trip: %s", strings.Join(problems, "\n"))
	}
}

func TestEvaluateRejectsAcceptanceAsRecovery(t *testing.T) {
	report := passingKindReport()
	for i := range report.Scenarios {
		if report.Scenarios[i].Name != ScenarioIdleToGame {
			continue
		}
		for j := range report.Scenarios[i].Samples {
			if report.Scenarios[i].Samples[j].Phase == PhaseEviction {
				report.Scenarios[i].Samples[j].SatisfiesRecovery = true
			}
		}
	}
	problems := Evaluate(report)
	if !containsProblem(problems, "eviction acceptance cannot satisfy recovery") {
		t.Fatalf("problems: %s", strings.Join(problems, "\n"))
	}
}

func TestEvaluateRejectsReleaseWithoutPodAbsence(t *testing.T) {
	report := passingKindReport()
	sample := releaseSample(report.Scenarios[0])
	sample.Evidence = EvidenceRequestsDropped + " " + EvidenceStillBound
	replaceRelease(&report.Scenarios[0], sample)
	problems := Evaluate(report)
	if !containsProblem(problems, EvidencePodAbsent) {
		t.Fatalf("problems: %s", strings.Join(problems, "\n"))
	}
}

func TestEvaluateRejectsShortAcceptanceGap(t *testing.T) {
	report := passingKindReport()
	sample := releaseSample(report.Scenarios[0])
	gap := int64(100)
	sample.AcceptanceGapMillis = &gap
	replaceRelease(&report.Scenarios[0], sample)
	problems := Evaluate(report)
	if !containsProblem(problems, "acceptance gap") {
		t.Fatalf("problems: %s", strings.Join(problems, "\n"))
	}
}

func TestEvaluateRejectsProtectedUIDChange(t *testing.T) {
	report := passingKindReport()
	for i := range report.Scenarios[0].Workloads {
		if report.Scenarios[0].Workloads[i].Name == WorkloadProtected {
			report.Scenarios[0].Workloads[i].UIDAfter = "replaced"
		}
	}
	problems := Evaluate(report)
	if !containsProblem(problems, "protected workload") {
		t.Fatalf("problems: %s", strings.Join(problems, "\n"))
	}
}

func TestEvaluateRejectsKindPerformanceClaim(t *testing.T) {
	report := passingKindReport()
	report.PerformanceClaimed = true
	report.GPUMeasured = true
	report.DesktopVisible = true
	problems := Evaluate(report)
	if !containsProblem(problems, "kind cannot set gpuMeasured") || !containsProblem(problems, "performanceClaimed is valid only for a live-gitops report") {
		t.Fatalf("problems: %s", strings.Join(problems, "\n"))
	}
}

func TestEvaluateRejectsPDBMarkedReleased(t *testing.T) {
	report := passingKindReport()
	for i := range report.Scenarios {
		if report.Scenarios[i].Name != ScenarioPDB {
			continue
		}
		for j := range report.Scenarios[i].Blocked {
			report.Scenarios[i].Blocked[j].ResourcesReleased = true
			report.Scenarios[i].Blocked[j].EvictionAccepted = true
		}
	}
	problems := Evaluate(report)
	if !containsProblem(problems, "must not accept eviction or release resources") {
		t.Fatalf("problems: %s", strings.Join(problems, "\n"))
	}
}

func TestEvaluateRejectsWeakenedBudget(t *testing.T) {
	report := passingKindReport()
	report.Budget.ResourceRelease = JSONDuration(time.Second)
	problems := Evaluate(report)
	if !containsProblem(problems, "budget.resourceRelease") {
		t.Fatalf("problems: %s", strings.Join(problems, "\n"))
	}
}

func TestCatalogQuantities(t *testing.T) {
	if len(Catalog()) != 6 {
		t.Fatalf("catalog = %d", len(Catalog()))
	}
	seen := map[string]bool{}
	for _, profile := range Catalog() {
		seen[profile.Name] = true
		if profile.GPU != 0 {
			t.Fatalf("%s requests a GPU", profile.Name)
		}
		if profile.CPUMillis <= 0 || profile.MemoryBytes <= 0 {
			t.Fatalf("%s has an empty size", profile.Name)
		}
	}
	for _, name := range []string{WorkloadLossy, WorkloadStandard, WorkloadProtected, WorkloadPinned, WorkloadPDB, WorkloadProbe} {
		if !seen[name] {
			t.Fatalf("missing %s", name)
		}
	}
	protected, _ := Lookup(WorkloadProtected)
	if protected.OptedIn || protected.Disposable {
		t.Fatal("protected neighbor must stay opted out")
	}
	pinned, _ := Lookup(WorkloadPinned)
	if !pinned.Pinned || !pinned.OptedIn || pinned.Disposable {
		t.Fatal("pinned neighbor must be opted in and not disposable")
	}
}

func passingKindReport() Report {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	report := Report{
		SchemaVersion:            SchemaVersion,
		Environment:              EnvKind,
		DetectionScope:           DetectionScopeController,
		Budget:                   DefaultBudget(),
		PerformedAt:              now,
		ClusterContext:           "kind-inc-recovery",
		NodeName:                 "inc-recovery-control-plane",
		Controller:               "in-process",
		GPUEvidence:              "kind has no GPU; device plugin and host samples were not collected",
		DesktopEvidence:          "kind has no desktop session",
		UnrelatedTaintPreserved:  true,
		ProtectedNamespaces:      []NamespaceSnapshot{{Namespace: "kube-system", PodUIDs: []string{"uid-a", "uid-b"}}},
		ProtectedNamespacesAfter: []NamespaceSnapshot{{Namespace: "kube-system", PodUIDs: []string{"uid-b", "uid-a"}}},
		WorkloadCatalog:          Catalog(),
		LiveRunRemaining:         KindLiveRunRemaining(),
	}
	for _, name := range RequiredScenarios() {
		report.Scenarios = append(report.Scenarios, passingScenario(name, now))
	}
	return report
}

func passingScenario(name string, now time.Time) ScenarioResult {
	scenario := ScenarioResult{Name: name, Workloads: baseWorkloads(name)}
	switch name {
	case ScenarioReturnToIdle:
		scenario.Samples = []PhaseSample{
			timedSample(name, PhaseDetection, TriggerSyntheticStatus, ExpectedObserved, "condition=host is idle", false, nil, now),
			timedSample(name, PhaseAdmissionReopen, TriggerSyntheticStatus, ExpectedReopened, "schedulable=true", false, nil, now),
			inactiveSample(name, PhaseEviction, TriggerSyntheticStatus),
			inactiveSample(name, PhaseRelease, TriggerSyntheticStatus),
		}
	case ScenarioControllerRestart:
		scenario.Samples = []PhaseSample{
			timedSample(name, PhaseLeaderElection, TriggerLeaderCancelled, ExpectedObserved, "new leader elected", false, nil, now),
			timedSample(name, PhaseLeaderRestore, TriggerDrift, ExpectedObserved, "managed taint restored", false, nil, now),
			timedSample(name, PhaseDetection, TriggerSyntheticStatus, ExpectedObserved, "condition=host is running an active game", false, nil, now),
			timedSample(name, PhaseAdmission, TriggerSyntheticStatus, ExpectedObserved, "admission closed", false, nil, now),
			timedSample(name, PhaseEviction, TriggerSyntheticStatus, ExpectedObserved, EvidenceDeletionTimestamp, false, nil, now),
			satisfyingRelease(name, TriggerSyntheticStatus, "pod-absent requests-dropped", now),
		}
	case ScenarioPDB:
		scenario.Blocked = []BlockedCase{{
			Workload: WorkloadPDB, Reason: "pdb", Evidence: EvidencePDB + " " + EvidenceStillBound,
		}}
		scenario.Samples = []PhaseSample{
			timedSample(name, PhaseDetection, TriggerSyntheticStatus, ExpectedObserved, "condition=host is interactive", false, nil, now),
			timedSample(name, PhaseAdmission, TriggerSyntheticStatus, ExpectedObserved, "admission closed", false, nil, now),
			timedSample(name, PhaseEviction, TriggerSyntheticStatus, ExpectedBlocked, EvidenceTimestampAbsent+" "+EvidencePDB, false, nil, now),
			timedSample(name, PhaseRelease, TriggerSyntheticStatus, ExpectedBlocked, EvidenceStillBound, false, nil, now),
			timedSample(name, PhaseEviction, TriggerPDBDeleted, ExpectedObserved, EvidenceDeletionTimestamp, false, nil, now),
			satisfyingRelease(name, TriggerPDBDeleted, EvidencePodAbsent+" "+EvidenceRequestsDropped+" "+EvidenceAfterPDBRemoved, now),
		}
	case ScenarioScheduledLead:
		start := now.Add(time.Minute)
		scenario.LeadTime = JSONDuration(50 * time.Second)
		scenario.WindowStart = &start
		scenario.Samples = recoverySamples(name, TriggerLeadBoundary, now)
	default:
		scenario.Samples = recoverySamples(name, TriggerSyntheticStatus, now)
		if name == ScenarioReporterFailure {
			scenario.Samples[0].Trigger = TriggerReporterStopped
			for i := range scenario.Samples {
				scenario.Samples[i].Trigger = TriggerReporterStopped
			}
		}
	}
	return scenario
}

func recoverySamples(scenario, trigger string, now time.Time) []PhaseSample {
	return []PhaseSample{
		timedSample(scenario, PhaseDetection, trigger, ExpectedObserved, "controller condition observed", false, nil, now),
		timedSample(scenario, PhaseAdmission, trigger, ExpectedObserved, "taint and unschedulable probe", false, nil, now),
		timedSample(scenario, PhaseEviction, trigger, ExpectedObserved, EvidenceDeletionTimestamp, false, nil, now),
		satisfyingRelease(scenario, trigger, EvidencePodAbsent+" "+EvidenceRequestsDropped, now),
	}
}

func baseWorkloads(scenario string) []WorkloadObservation {
	lossyEvict := scenario == ScenarioIdleToInteractive || scenario == ScenarioIdleToGame || scenario == ScenarioScheduledLead || scenario == ScenarioReporterFailure || scenario == ScenarioControllerRestart || scenario == ScenarioPDB
	standardEvict := scenario == ScenarioIdleToGame || scenario == ScenarioControllerRestart
	workloads := []WorkloadObservation{
		workloadObs(WorkloadLossy, lossyEvict, ""),
		workloadObs(WorkloadStandard, standardEvict, ""),
		workloadObs(WorkloadProtected, false, ""),
		workloadObs(WorkloadPinned, false, ""),
	}
	if scenario == ScenarioPDB {
		workloads = append(workloads, workloadObs(WorkloadPDB, false, "blocked"), workloadObs(WorkloadPDB, true, "cleared"))
	}
	return workloads
}

func workloadObs(name string, evicted bool, stage string) WorkloadObservation {
	profile, _ := Lookup(name)
	uid := name + "-before"
	after, phase, holds := uid, "Running", true
	if evicted {
		after, phase, holds = "", "Absent", false
	}
	return WorkloadObservation{
		Name: profile.Name, Role: profile.Role, Disposable: profile.Disposable, OptedIn: profile.OptedIn,
		Lossy: profile.Lossy, Pinned: profile.Pinned, CPUMillis: profile.CPUMillis, MemoryBytes: profile.MemoryBytes,
		UIDBefore: uid, UIDAfter: after, PhaseAfter: phase, HoldsResourcesAfter: holds, EvictedDuringScenario: evicted, Stage: stage,
	}
}

func timedSample(scenario, phase, trigger, expected, evidence string, satisfies bool, gap *int64, at time.Time) PhaseSample {
	observed := at.Add(time.Second)
	duration := int64(1000)
	return PhaseSample{
		Phase: phase, Applicable: true, Expected: expected, Trigger: trigger, TriggerAt: at, ObservedAt: &observed,
		DurationMillis: &duration, Budget: JSONDuration(AllowedBudget(DefaultBudget(), scenario, phase)),
		WithinBudget: true, SatisfiesRecovery: satisfies, AcceptanceGapMillis: gap, Evidence: evidence,
	}
}

func satisfyingRelease(scenario, trigger, evidence string, at time.Time) PhaseSample {
	gap := int64(3000)
	return timedSample(scenario, PhaseRelease, trigger, ExpectedObserved, evidence, true, &gap, at)
}

func inactiveSample(scenario, phase, trigger string) PhaseSample {
	return PhaseSample{
		Phase: phase, Expected: ExpectedAbsent, Trigger: trigger, Budget: JSONDuration(AllowedBudget(DefaultBudget(), scenario, phase)),
		WithinBudget: true, Evidence: EvidenceUIDUnchanged,
	}
}

func releaseSample(scenario ScenarioResult) PhaseSample {
	for _, sample := range scenario.Samples {
		if sample.Phase == PhaseRelease && sample.SatisfiesRecovery {
			return sample
		}
	}
	panic("missing release sample")
}

func replaceRelease(scenario *ScenarioResult, sample PhaseSample) {
	for i := range scenario.Samples {
		if scenario.Samples[i].Phase == PhaseRelease && scenario.Samples[i].SatisfiesRecovery {
			scenario.Samples[i] = sample
			return
		}
	}
}

func containsProblem(problems []string, needle string) bool {
	for _, problem := range problems {
		if strings.Contains(problem, needle) {
			return true
		}
	}
	return false
}

func TestEvaluateRejectsEvictionOfNonDisposableWorkload(t *testing.T) {
	report := passingKindReport()
	for i := range report.Scenarios[0].Workloads {
		w := &report.Scenarios[0].Workloads[i]
		if w.Name == WorkloadLossy && w.EvictedDuringScenario {
			// Evicted, yet the observation says it still holds its requests.
			w.HoldsResourcesAfter = true
		}
	}
	problems := Evaluate(report)
	if !containsProblem(problems, "eviction breaks the disposable opt-in contract") {
		t.Fatalf("problems: %s", strings.Join(problems, "\n"))
	}
}

func TestEvaluateRejectsGatedSurvivorReleasingRequests(t *testing.T) {
	report := passingKindReport()
	found := false
	for i := range report.Scenarios[0].Workloads {
		w := &report.Scenarios[0].Workloads[i]
		if w.Name == WorkloadStandard && !w.EvictedDuringScenario {
			w.HoldsResourcesAfter = false
			found = true
		}
	}
	if !found {
		t.Fatalf("scenario %s has no surviving standard workload", report.Scenarios[0].Name)
	}
	problems := Evaluate(report)
	if !containsProblem(problems, "should still hold its requests") {
		t.Fatalf("problems: %s", strings.Join(problems, "\n"))
	}
}
