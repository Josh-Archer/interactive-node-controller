package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const legacyV015NodeActivityCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: nodeactivities.availability.interactive-node.io
spec:
  group: availability.interactive-node.io
  scope: Namespaced
  names:
    kind: NodeActivity
    listKind: NodeActivityList
    plural: nodeactivities
    singular: nodeactivity
    shortNames: [nodeactivity, na]
  versions:
    - name: v1alpha1
      served: true
      storage: true
      subresources: {status: {}}
      schema:
        openAPIV3Schema:
          type: object
          required: [spec]
          properties:
            spec:
              type: object
              required: [nodeName]
              x-kubernetes-validations:
                - rule: oldSelf == null || self.nodeName == oldSelf.nodeName
                  message: spec.nodeName is immutable after enrollment
              properties:
                nodeName:
                  type: string
                  minLength: 1
                  maxLength: 253
                  pattern: '^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$'
                  description: Exact Kubernetes Node name enrolled by GitOps.
            status:
              type: object
              properties:
                state: {type: string, enum: [active, idle, unknown, stale]}
                activity: {type: string, enum: [game, interactive, idle, unknown]}
                reason: {type: string, maxLength: 1024}
                observedAt: {type: string, format: date-time}
                heartbeatAt: {type: string, format: date-time}
                generation: {type: integer, format: int64, minimum: 0}
                failClosed: {type: boolean}
                managedTaint:
                  type: object
                  required: [key, value, effect]
                  properties:
                    key: {type: string}
                    value: {type: string}
                    effect: {type: string, enum: [NoSchedule, PreferNoSchedule]}
                conditions:
                  type: array
                  x-kubernetes-list-type: map
                  x-kubernetes-list-map-keys: [type]
                  items:
                    type: object
                    required: [type, status, reason, message, lastTransitionTime]
                    properties:
                      type: {type: string}
                      status: {type: string, enum: ['True', 'False', 'Unknown']}
                      observedGeneration: {type: integer, format: int64}
                      lastTransitionTime: {type: string, format: date-time}
                      reason: {type: string}
                      message: {type: string}
`

const legacyV016NodeActivityCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: nodeactivities.availability.interactive-node.io
spec:
  group: availability.interactive-node.io
  scope: Namespaced
  names:
    kind: NodeActivity
    listKind: NodeActivityList
    plural: nodeactivities
    singular: nodeactivity
    shortNames: [nodeactivity, na]
  versions:
    - name: v1alpha1
      served: true
      storage: true
      subresources: {status: {}}
      schema:
        openAPIV3Schema:
          type: object
          required: [spec]
          properties:
            spec:
              type: object
              required: [nodeName]
              x-kubernetes-validations:
                - rule: oldSelf == null || self.nodeName == oldSelf.nodeName
                  message: spec.nodeName is immutable after enrollment
              properties:
                nodeName:
                  type: string
                  minLength: 1
                  maxLength: 253
                  pattern: '^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$'
                  description: Exact Kubernetes Node name enrolled by GitOps.
                leadTime:
                  type: string
                  pattern: '^([0-9]+(ns|us|µs|ms|s|m|h))+$'
                  description: Duration before a window starts to apply its managed taint.
                windows:
                  type: array
                  items:
                    type: object
                    required: [schedule, duration, state]
                    properties:
                      schedule: {type: string, minLength: 1}
                      duration: {type: string, pattern: '^([0-9]+(ns|us|µs|ms|s|m|h))+$'}
                      timeZone: {type: string}
                      state: {type: string, enum: [interactive, game, idle-allowed]}
            status:
              type: object
              properties:
                state: {type: string, enum: [active, idle, unknown, stale]}
                activity: {type: string, enum: [game, interactive, idle, unknown]}
                reason: {type: string, maxLength: 1024}
                observedAt: {type: string, format: date-time}
                heartbeatAt: {type: string, format: date-time}
                generation: {type: integer, format: int64, minimum: 0}
                failClosed: {type: boolean}
                managedTaint:
                  type: object
                  required: [key, value, effect]
                  properties:
                    key: {type: string}
                    value: {type: string}
                    effect: {type: string, enum: [NoSchedule, PreferNoSchedule]}
                conditions:
                  type: array
                  x-kubernetes-list-type: map
                  x-kubernetes-list-map-keys: [type]
                  items:
                    type: object
                    required: [type, status, reason, message, lastTransitionTime]
                    properties:
                      type: {type: string}
                      status: {type: string, enum: ['True', 'False', 'Unknown']}
                      observedGeneration: {type: integer, format: int64}
                      lastTransitionTime: {type: string, format: date-time}
                      reason: {type: string}
                      message: {type: string}
`

func decodeCRDManifest(t *testing.T, raw []byte) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096).Decode(crd); err != nil {
		t.Fatalf("decode CRD manifest: %v", err)
	}
	return crd
}

func loadCurrentCRDManifest(t *testing.T, relPath string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", relPath))
	if err != nil {
		t.Fatalf("read %s: %v", relPath, err)
	}
	return decodeCRDManifest(t, raw)
}

func TestCRDSchemaReadinessCurrentManifestsPass(t *testing.T) {
	for _, relPath := range []string{
		"config/crd/bases/availability.interactive-node.io_nodeactivities.yaml",
		"chart/crds/availability.interactive-node.io_nodeactivities.yaml",
	} {
		crd := loadCurrentCRDManifest(t, relPath)
		if got := crd.Annotations["argocd.argoproj.io/sync-wave"]; got != "-1" {
			t.Fatalf("%s argocd.argoproj.io/sync-wave = %q, want \"-1\"", relPath, got)
		}
		if err := ValidateNodeActivityCRD(crd); err != nil {
			t.Fatalf("%s failed ValidateNodeActivityCRD: %v", relPath, err)
		}
	}
}

func TestCRDSchemaReadinessRejectsStaleSchemasAndTransitionsReadyOnUpgrade(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	// 1. Missing CRD fails readiness clearly.
	emptyClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	check := NewCRDSchemaReadyzCheck(emptyClient)
	if err := check(req); err == nil || !strings.Contains(err.Error(), "is not installed") {
		t.Fatalf("missing CRD error = %v, want not installed message", err)
	}

	// 2. Previous release v0.1.5 CRD (missing spec.leadTime, spec.windows, status.admission) fails readiness clearly.
	v015 := decodeCRDManifest(t, []byte(legacyV015NodeActivityCRD))
	clientWithCRD := fake.NewClientBuilder().WithScheme(scheme).WithObjects(v015).Build()
	check = NewCRDSchemaReadyzCheck(clientWithCRD)
	err := check(req)
	if err == nil {
		t.Fatal("expected v0.1.5 CRD schema to fail readiness")
	}
	for _, want := range []string{
		"schema is older than expected",
		"spec.leadTime",
		"spec.windows",
		"status.admission",
		"apply the current CRD before upgrading the controller",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("v0.1.5 readiness error %q missing %q", err.Error(), want)
		}
	}

	// 3. Intermediate v0.1.6 CRD (has spec.leadTime and spec.windows, missing status.admission) fails readiness clearly.
	v016 := decodeCRDManifest(t, []byte(legacyV016NodeActivityCRD))
	v016.ResourceVersion = v015.ResourceVersion
	if err := clientWithCRD.Update(req.Context(), v016); err != nil {
		t.Fatal(err)
	}
	err = check(req)
	if err == nil {
		t.Fatal("expected v0.1.6 CRD schema to fail readiness")
	}
	if !strings.Contains(err.Error(), "missing fields [status.admission]") {
		t.Fatalf("v0.1.6 readiness error = %q, want missing fields [status.admission]", err.Error())
	}

	// 4. Upgrading to the current chart CRD makes readiness succeed immediately.
	current := loadCurrentCRDManifest(t, "chart/crds/availability.interactive-node.io_nodeactivities.yaml")
	current.ResourceVersion = v016.ResourceVersion
	if err := clientWithCRD.Update(req.Context(), current); err != nil {
		t.Fatal(err)
	}
	if err := check(req); err != nil {
		t.Fatalf("current CRD failed readiness after upgrade: %v", err)
	}
}

func TestValidateNodeActivityCRDEdgeCases(t *testing.T) {
	current := loadCurrentCRDManifest(t, "chart/crds/availability.interactive-node.io_nodeactivities.yaml")

	// Unestablished condition fails readiness when status conditions are present.
	unestablished := current.DeepCopy()
	unestablished.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{
		{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionFalse},
	}
	if err := ValidateNodeActivityCRD(unestablished); err == nil || !strings.Contains(err.Error(), "not yet Established") {
		t.Fatalf("unestablished CRD error = %v, want not yet Established", err)
	}

	// Missing nested window property fails readiness naming the exact nested path.
	missingTimezone := current.DeepCopy()
	delete(missingTimezone.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["windows"].Items.Schema.Properties, "timeZone")
	if err := ValidateNodeActivityCRD(missingTimezone); err == nil || !strings.Contains(err.Error(), "spec.windows[].timeZone") {
		t.Fatalf("missing spec.windows[].timeZone error = %v", err)
	}

	// Missing nested burstTaint property fails readiness naming the exact nested path.
	missingBurst := current.DeepCopy()
	delete(missingBurst.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"].Properties["admission"].Properties["burstTaint"].Properties, "applied")
	if err := ValidateNodeActivityCRD(missingBurst); err == nil || !strings.Contains(err.Error(), "status.admission.burstTaint.applied") {
		t.Fatalf("missing status.admission.burstTaint.applied error = %v", err)
	}

	// Missing status subresource fails readiness.
	noStatusSubresource := current.DeepCopy()
	noStatusSubresource.Spec.Versions[0].Subresources = nil
	if err := ValidateNodeActivityCRD(noStatusSubresource); err == nil || !strings.Contains(err.Error(), "missing the status subresource") {
		t.Fatalf("missing status subresource error = %v", err)
	}
}
