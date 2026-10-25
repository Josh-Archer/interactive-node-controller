package recovery

import (
	"os"
	"strings"
	"testing"
)

func TestExampleMatchesCatalog(t *testing.T) {
	raw, err := os.ReadFile("../../examples/recovery-workloads.yaml")
	if err != nil {
		t.Fatal(err)
	}
	docs := strings.Split(string(raw), "\n---\n")
	find := func(kind, name string) string {
		t.Helper()
		for _, doc := range docs {
			if strings.Contains(doc, "kind: "+kind) && strings.Contains(doc, "\n  name: "+name+"\n") {
				return doc
			}
		}
		t.Fatalf("missing %s %s", kind, name)
		return ""
	}
	lossy := find("Deployment", WorkloadLossy)
	if !strings.Contains(lossy, `interactive-node-controller.io/evictable: "true"`) || !strings.Contains(lossy, `interactive-node-controller.io/allow-lossy-eviction: "true"`) || !strings.Contains(lossy, "cpu: 100m") || !strings.Contains(lossy, "memory: 64Mi") || !strings.Contains(lossy, `availability.interactive-node.io/schedulable: "true"`) {
		t.Fatal("lossy-runner manifest drifted from the catalog")
	}
	standard := find("Deployment", WorkloadStandard)
	if !strings.Contains(standard, `interactive-node-controller.io/evictable: "true"`) || strings.Contains(standard, "allow-lossy-eviction") || !strings.Contains(standard, "cpu: 50m") || !strings.Contains(standard, "memory: 32Mi") || strings.Contains(standard, "nodeSelector:") {
		t.Fatal("standard-runner manifest drifted from the catalog")
	}
	protected := find("Deployment", WorkloadProtected)
	if strings.Contains(protected, "evictable") || strings.Contains(protected, "allow-lossy-eviction") || !strings.Contains(protected, "cpu: 200m") || !strings.Contains(protected, "memory: 128Mi") {
		t.Fatal("protected-neighbor manifest drifted from the catalog")
	}
	pinned := find("Deployment", WorkloadPinned)
	if !strings.Contains(pinned, `interactive-node-controller.io/evictable: "true"`) || !strings.Contains(pinned, `interactive-node-controller.io/pinned: "true"`) || strings.Contains(pinned, "allow-lossy-eviction") || !strings.Contains(pinned, "cpu: 20m") || !strings.Contains(pinned, "memory: 16Mi") {
		t.Fatal("pinned-neighbor manifest drifted from the catalog")
	}
	pdb := find("Deployment", WorkloadPDB)
	if !strings.Contains(pdb, `interactive-node-controller.io/allow-lossy-eviction: "true"`) || !strings.Contains(pdb, "cpu: 80m") || !strings.Contains(pdb, "memory: 48Mi") || !strings.Contains(pdb, `availability.interactive-node.io/schedulable: "true"`) {
		t.Fatal("pdb-runner manifest drifted from the catalog")
	}
	budget := find("PodDisruptionBudget", WorkloadPDB)
	if !strings.Contains(budget, "minAvailable: 1") {
		t.Fatal("pdb-runner budget drifted")
	}
	probe := find("Pod", WorkloadProbe)
	if strings.Contains(probe, "evictable") || !strings.Contains(probe, "cpu: 10m") || !strings.Contains(probe, "memory: 8Mi") || !strings.Contains(probe, `availability.interactive-node.io/schedulable: "true"`) {
		t.Fatal("admission-probe manifest drifted from the catalog")
	}
}
