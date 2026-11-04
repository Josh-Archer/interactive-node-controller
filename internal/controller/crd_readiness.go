package controller

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

const (
	// NodeActivityCRDName is the canonical CustomResourceDefinition name for NodeActivity.
	NodeActivityCRDName = "nodeactivities.availability.interactive-node.io"

	crdSchemaCheckTimeout = 5 * time.Second
)

// NewCRDSchemaReadyzCheck returns a controller-runtime readiness check that fails
// clearly when the installed NodeActivity CRD is missing, not yet Established, or
// serving a schema older than this controller expects.
func NewCRDSchemaReadyzCheck(reader client.Reader) healthz.Checker {
	return func(req *http.Request) error {
		baseCtx := context.Background()
		if req != nil && req.Context() != nil {
			baseCtx = req.Context()
		}
		ctx, cancel := context.WithTimeout(baseCtx, crdSchemaCheckTimeout)
		defer cancel()
		return CheckNodeActivityCRDSchema(ctx, reader)
	}
}

// CheckNodeActivityCRDSchema reads the live NodeActivity CustomResourceDefinition
// from the API server and verifies that its schema includes every field this
// controller release relies on.
func CheckNodeActivityCRDSchema(ctx context.Context, reader client.Reader) error {
	if reader == nil {
		return fmt.Errorf("CRD schema check requires a Kubernetes API reader")
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := reader.Get(ctx, types.NamespacedName{Name: NodeActivityCRDName}, crd); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("CRD %s is not installed; apply the current CRD before starting the controller", NodeActivityCRDName)
		}
		return fmt.Errorf("read CRD %s: %w", NodeActivityCRDName, err)
	}
	return ValidateNodeActivityCRD(crd)
}

// ValidateNodeActivityCRD checks that the given CustomResourceDefinition serves
// v1alpha1 with the status subresource and all expected spec and status properties.
func ValidateNodeActivityCRD(crd *apiextensionsv1.CustomResourceDefinition) error {
	if crd == nil {
		return fmt.Errorf("CRD %s is nil", NodeActivityCRDName)
	}
	if crd.Name != "" && crd.Name != NodeActivityCRDName {
		return fmt.Errorf("unexpected CRD name %q, want %q", crd.Name, NodeActivityCRDName)
	}
	if len(crd.Status.Conditions) > 0 && !crdIsEstablished(crd) {
		return fmt.Errorf("CRD %s is not yet Established", NodeActivityCRDName)
	}
	var served *apiextensionsv1.CustomResourceDefinitionVersion
	for i := range crd.Spec.Versions {
		v := &crd.Spec.Versions[i]
		if v.Name == availabilityv1alpha1.Version {
			served = v
			break
		}
	}
	if served == nil || !served.Served {
		return fmt.Errorf("installed CRD %s does not serve version %s; apply the current CRD before upgrading the controller", NodeActivityCRDName, availabilityv1alpha1.Version)
	}
	if served.Subresources == nil || served.Subresources.Status == nil {
		return fmt.Errorf("installed CRD %s (%s) is missing the status subresource; apply the current CRD before upgrading the controller", NodeActivityCRDName, availabilityv1alpha1.Version)
	}
	if served.Schema == nil || served.Schema.OpenAPIV3Schema == nil {
		return fmt.Errorf("installed CRD %s (%s) is missing openAPIV3Schema; apply the current CRD before upgrading the controller", NodeActivityCRDName, availabilityv1alpha1.Version)
	}
	missing := missingNodeActivitySchemaFields(served.Schema.OpenAPIV3Schema)
	if len(missing) > 0 {
		return fmt.Errorf("installed CRD %s (%s) schema is older than expected: missing fields [%s]; apply the current CRD before upgrading the controller", NodeActivityCRDName, availabilityv1alpha1.Version, strings.Join(missing, ", "))
	}
	return nil
}

func crdIsEstablished(crd *apiextensionsv1.CustomResourceDefinition) bool {
	for _, cond := range crd.Status.Conditions {
		if cond.Type == apiextensionsv1.Established {
			return cond.Status == apiextensionsv1.ConditionTrue
		}
	}
	return false
}

func missingNodeActivitySchemaFields(root *apiextensionsv1.JSONSchemaProps) []string {
	var missing []string

	spec, ok := lookupProperty(root, "spec")
	if !ok {
		missing = append(missing, "spec")
	} else {
		requireProperties(spec, "spec", []string{"nodeName", "leadTime"}, &missing)
		if windows, hasWindows := lookupProperty(spec, "windows"); !hasWindows {
			missing = append(missing, "spec.windows")
		} else if windows.Items == nil || windows.Items.Schema == nil {
			missing = append(missing, "spec.windows[]")
		} else {
			requireProperties(windows.Items.Schema, "spec.windows[]", []string{"schedule", "duration", "timeZone", "state"}, &missing)
		}
	}

	status, ok := lookupProperty(root, "status")
	if !ok {
		missing = append(missing, "status")
	} else {
		requireProperties(status, "status", []string{
			"state",
			"activity",
			"reason",
			"observedAt",
			"heartbeatAt",
			"generation",
			"failClosed",
		}, &missing)

		if managedTaint, hasManagedTaint := lookupProperty(status, "managedTaint"); !hasManagedTaint {
			missing = append(missing, "status.managedTaint")
		} else {
			requireProperties(managedTaint, "status.managedTaint", []string{"key", "value", "effect"}, &missing)
		}

		if admission, hasAdmission := lookupProperty(status, "admission"); !hasAdmission {
			missing = append(missing, "status.admission")
		} else {
			requireProperties(admission, "status.admission", []string{
				"admitted",
				"reason",
				"message",
				"heartbeatValid",
				"heartbeatAgeSeconds",
			}, &missing)
			if burst, hasBurst := lookupProperty(admission, "burstTaint"); !hasBurst {
				missing = append(missing, "status.admission.burstTaint")
			} else {
				requireProperties(burst, "status.admission.burstTaint", []string{"configured", "desired", "applied"}, &missing)
			}
			requireProperties(admission, "status.admission", []string{"nextTransition", "nextTransitionReason"}, &missing)
		}

		if conditions, hasConditions := lookupProperty(status, "conditions"); !hasConditions {
			missing = append(missing, "status.conditions")
		} else if conditions.Items == nil || conditions.Items.Schema == nil {
			missing = append(missing, "status.conditions[]")
		} else {
			requireProperties(conditions.Items.Schema, "status.conditions[]", []string{
				"type",
				"status",
				"observedGeneration",
				"lastTransitionTime",
				"reason",
				"message",
			}, &missing)
		}
	}

	return missing
}

func lookupProperty(schema *apiextensionsv1.JSONSchemaProps, name string) (*apiextensionsv1.JSONSchemaProps, bool) {
	if schema == nil || schema.Properties == nil {
		return nil, false
	}
	prop, ok := schema.Properties[name]
	if !ok {
		return nil, false
	}
	return &prop, true
}

func requireProperties(schema *apiextensionsv1.JSONSchemaProps, prefix string, names []string, missing *[]string) {
	for _, name := range names {
		if _, ok := lookupProperty(schema, name); !ok {
			*missing = append(*missing, prefix+"."+name)
		}
	}
}
