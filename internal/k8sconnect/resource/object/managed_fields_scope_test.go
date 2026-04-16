package object

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/jmorris0x0/terraform-provider-k8sconnect/internal/k8sconnect/common/fieldmanagement"
)

// buildFieldsV1 turns a map of JSON paths (f:a.f:b) into the FieldsV1 format
// Kubernetes uses for managedFields entries.
func buildFieldsV1(t *testing.T, fields map[string]interface{}) []byte {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal FieldsV1: %v", err)
	}
	return raw
}

// TestComputeRelevantManagedFieldPaths_ExcludesUnrelatedExternalPaths verifies
// that when an external manager owns a path k8sconnect has never referenced
// (not in yaml_body, not currently owned by k8sconnect, not in baseline), the
// path is NOT considered relevant. This is the core ADR-024 guarantee that
// prevents "Provider produced inconsistent result after apply" errors when
// controllers like ArgoCD write `operation.*` on an Application CR between
// our plan and apply.
func TestComputeRelevantManagedFieldPaths_ExcludesUnrelatedExternalPaths(t *testing.T) {
	yamlBody := `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: test-app
  namespace: argocd
spec:
  project: default
`

	// External controller (argocd-application-controller) owns operation.sync.
	// k8sconnect has never touched operation.* and it's not in yaml_body.
	ownership := map[string][]string{
		"apiVersion":                  {"k8sconnect"},
		"kind":                        {"k8sconnect"},
		"metadata.name":               {"k8sconnect"},
		"metadata.namespace":          {"k8sconnect"},
		"spec.project":                {"k8sconnect"},
		"operation.sync.revision":     {"argocd-application-controller"},
		"operation.sync.prune":        {"argocd-application-controller"},
		"status.operationState.phase": {"argocd-application-controller"},
	}

	relevant := computeRelevantManagedFieldPaths(yamlBody, nil, ownership)

	// yaml_body + k8sconnect ownership paths should be relevant.
	for _, p := range []string{"apiVersion", "kind", "metadata.name", "metadata.namespace", "spec.project"} {
		if !relevant[p] {
			t.Errorf("expected %q to be relevant (in yaml_body / owned by k8sconnect), but it was excluded", p)
		}
	}

	// Foreign paths k8sconnect has no relationship with must be excluded.
	for _, p := range []string{"operation.sync.revision", "operation.sync.prune", "status.operationState.phase"} {
		if relevant[p] {
			t.Errorf("expected %q to be excluded (foreign controller, not in yaml_body), but it was kept", p)
		}
	}
}

// TestComputeRelevantManagedFieldPaths_IncludesYamlBodyPathsEvenWhenExternallyOwned
// verifies that when an external manager takes over a path that IS in yaml_body,
// the path stays in scope so the ownership transition is visible to the user.
func TestComputeRelevantManagedFieldPaths_IncludesYamlBodyPathsEvenWhenExternallyOwned(t *testing.T) {
	yamlBody := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-deploy
  namespace: default
spec:
  replicas: 3
`

	// External HPA has taken over spec.replicas — but it's still in yaml_body.
	ownership := map[string][]string{
		"apiVersion":         {"k8sconnect"},
		"kind":               {"k8sconnect"},
		"metadata.name":      {"k8sconnect"},
		"metadata.namespace": {"k8sconnect"},
		"spec.replicas":      {"hpa-controller"},
	}

	relevant := computeRelevantManagedFieldPaths(yamlBody, nil, ownership)

	if !relevant["spec.replicas"] {
		t.Errorf("expected spec.replicas to be relevant (it's in yaml_body), but it was excluded")
	}
}

// TestComputeRelevantManagedFieldPaths_IncludesBaselinePathsOnTransition verifies
// that a path k8sconnect previously owned (per the ADR-021 baseline) stays in
// scope even when another manager now owns it and it's no longer in yaml_body.
// This keeps the transition visible across the first apply after the user
// removed the field from yaml_body.
func TestComputeRelevantManagedFieldPaths_IncludesBaselinePathsOnTransition(t *testing.T) {
	// yaml_body no longer contains spec.replicas (user removed it).
	yamlBody := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-deploy
  namespace: default
`

	// Baseline says we owned spec.replicas at last apply.
	baseline := map[string]string{
		"spec.replicas": "k8sconnect",
	}

	// Current ownership: HPA now owns it.
	ownership := map[string][]string{
		"apiVersion":         {"k8sconnect"},
		"kind":               {"k8sconnect"},
		"metadata.name":      {"k8sconnect"},
		"metadata.namespace": {"k8sconnect"},
		"spec.replicas":      {"hpa-controller"},
	}

	relevant := computeRelevantManagedFieldPaths(yamlBody, baseline, ownership)

	if !relevant["spec.replicas"] {
		t.Errorf("expected spec.replicas to be relevant (previously owned by k8sconnect per baseline), but it was excluded")
	}
}

// TestComputeRelevantManagedFieldPaths_BaselineForeignOwnerDoesNotExpandScope
// verifies that a baseline path whose previous owner was NOT k8sconnect does
// not by itself force the path into scope. The baseline stores first-manager
// for drift detection, but only k8sconnect-owned history should expand our
// managed_fields scope.
func TestComputeRelevantManagedFieldPaths_BaselineForeignOwnerDoesNotExpandScope(t *testing.T) {
	yamlBody := `apiVersion: v1
kind: ConfigMap
metadata:
  name: test-cm
  namespace: default
data:
  key: value
`

	baseline := map[string]string{
		// A path outside yaml_body that was previously owned by kubectl.
		"data.external-key": "kubectl",
	}

	ownership := map[string][]string{
		"apiVersion":         {"k8sconnect"},
		"kind":               {"k8sconnect"},
		"metadata.name":      {"k8sconnect"},
		"metadata.namespace": {"k8sconnect"},
		"data.key":           {"k8sconnect"},
		"data.external-key":  {"kubectl"},
	}

	relevant := computeRelevantManagedFieldPaths(yamlBody, baseline, ownership)

	if relevant["data.external-key"] {
		t.Errorf("expected data.external-key to be excluded (baseline owner was not k8sconnect, not in yaml_body), but it was kept")
	}
	if !relevant["data.key"] {
		t.Errorf("expected data.key to be relevant (in yaml_body), but it was excluded")
	}
}

// TestUpdateManagedFieldsData_ExcludesForeignExternalPaths is an end-to-end
// check on updateManagedFieldsData: after scoping, the produced managed_fields
// map must not contain foreign paths outside k8sconnect's relationship.
func TestUpdateManagedFieldsData_ExcludesForeignExternalPaths(t *testing.T) {
	ctx := context.Background()

	yamlBody := `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: test-app
  namespace: argocd
spec:
  project: default
`

	// Simulated live object: k8sconnect owns metadata + spec.project, ArgoCD
	// controller owns operation.* (a field path outside the user's yaml_body).
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "argoproj.io/v1alpha1",
			"kind":       "Application",
			"metadata": map[string]interface{}{
				"name":      "test-app",
				"namespace": "argocd",
			},
			"spec": map[string]interface{}{
				"project": "default",
			},
			"operation": map[string]interface{}{
				"sync": map[string]interface{}{
					"revision": "abc123",
				},
			},
		},
	}
	obj.SetManagedFields([]metav1.ManagedFieldsEntry{
		{
			Manager:   "k8sconnect",
			Operation: metav1.ManagedFieldsOperationApply,
			FieldsV1: &metav1.FieldsV1{
				Raw: buildFieldsV1(t, map[string]interface{}{
					"f:metadata": map[string]interface{}{
						"f:name":      map[string]interface{}{},
						"f:namespace": map[string]interface{}{},
					},
					"f:spec": map[string]interface{}{
						"f:project": map[string]interface{}{},
					},
				}),
			},
		},
		{
			Manager:   "argocd-application-controller",
			Operation: metav1.ManagedFieldsOperationUpdate,
			FieldsV1: &metav1.FieldsV1{
				Raw: buildFieldsV1(t, map[string]interface{}{
					"f:operation": map[string]interface{}{
						"f:sync": map[string]interface{}{
							"f:revision": map[string]interface{}{},
						},
					},
				}),
			},
		},
	})

	data := &objectResourceModel{
		IgnoreFields: types.ListNull(types.StringType),
	}

	updateManagedFieldsData(ctx, data, obj, yamlBody, nil)

	var mf map[string]string
	diags := data.ManagedFields.ElementsAs(ctx, &mf, false)
	if diags.HasError() {
		t.Fatalf("failed to extract managed_fields: %v", diags)
	}

	if _, present := mf["operation.sync.revision"]; present {
		t.Errorf("managed_fields must NOT contain foreign path operation.sync.revision; got: %#v", mf)
	}
	if mf["spec.project"] != "k8sconnect" {
		t.Errorf("expected managed_fields[spec.project] == k8sconnect, got %q (full map: %#v)", mf["spec.project"], mf)
	}
}

// TestUpdateManagedFieldsData_TransitionOnYamlBodyPathStaysVisible verifies
// that when an external manager takes over a path that is in yaml_body, the
// ownership transition remains visible in managed_fields (external manager's
// name surfaced, not k8sconnect).
func TestUpdateManagedFieldsData_TransitionOnYamlBodyPathStaysVisible(t *testing.T) {
	ctx := context.Background()

	yamlBody := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-deploy
  namespace: default
spec:
  replicas: 3
`

	// hpa-controller took over spec.replicas; k8sconnect still in baseline/yaml.
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]interface{}{
				"name":      "test-deploy",
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"replicas": int64(5),
			},
		},
	}
	obj.SetManagedFields([]metav1.ManagedFieldsEntry{
		{
			Manager:   "k8sconnect",
			Operation: metav1.ManagedFieldsOperationApply,
			FieldsV1: &metav1.FieldsV1{
				Raw: buildFieldsV1(t, map[string]interface{}{
					"f:metadata": map[string]interface{}{
						"f:name":      map[string]interface{}{},
						"f:namespace": map[string]interface{}{},
					},
				}),
			},
		},
		{
			Manager:   "hpa-controller",
			Operation: metav1.ManagedFieldsOperationApply,
			FieldsV1: &metav1.FieldsV1{
				Raw: buildFieldsV1(t, map[string]interface{}{
					"f:spec": map[string]interface{}{
						"f:replicas": map[string]interface{}{},
					},
				}),
			},
		},
	})

	baseline := map[string]string{
		"spec.replicas": "k8sconnect",
	}

	data := &objectResourceModel{
		IgnoreFields: types.ListNull(types.StringType),
	}

	updateManagedFieldsData(ctx, data, obj, yamlBody, baseline)

	var mf map[string]string
	diags := data.ManagedFields.ElementsAs(ctx, &mf, false)
	if diags.HasError() {
		t.Fatalf("failed to extract managed_fields: %v", diags)
	}

	owner, ok := mf["spec.replicas"]
	if !ok {
		t.Fatalf("expected managed_fields to include spec.replicas so the ownership transition is visible, got: %#v", mf)
	}
	if owner != "hpa-controller" {
		t.Errorf("expected managed_fields[spec.replicas] == hpa-controller (external took over), got %q", owner)
	}
}

// TestUpdateManagedFieldsData_StatusFilterStillApplies verifies that the
// existing status.* filter continues to exclude status fields even when they
// fall within the relevant scope (e.g., k8sconnect briefly owns a status
// field via a controller quirk).
func TestUpdateManagedFieldsData_StatusFilterStillApplies(t *testing.T) {
	ctx := context.Background()

	yamlBody := `apiVersion: v1
kind: Pod
metadata:
  name: test-pod
  namespace: default
`

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "test-pod",
				"namespace": "default",
			},
		},
	}
	obj.SetManagedFields([]metav1.ManagedFieldsEntry{
		{
			Manager:   "k8sconnect",
			Operation: metav1.ManagedFieldsOperationApply,
			FieldsV1: &metav1.FieldsV1{
				Raw: buildFieldsV1(t, map[string]interface{}{
					"f:metadata": map[string]interface{}{
						"f:name":      map[string]interface{}{},
						"f:namespace": map[string]interface{}{},
					},
					"f:status": map[string]interface{}{
						"f:phase": map[string]interface{}{},
					},
				}),
			},
		},
	})

	data := &objectResourceModel{
		IgnoreFields: types.ListNull(types.StringType),
	}

	updateManagedFieldsData(ctx, data, obj, yamlBody, nil)

	var mf map[string]string
	_ = data.ManagedFields.ElementsAs(ctx, &mf, false)

	if _, present := mf["status.phase"]; present {
		t.Errorf("status.phase must be filtered out regardless of scope; got: %#v", mf)
	}
}

// Smoke-check fieldmanagement.ExtractAllManagedFields agrees with our
// FieldsV1 fixture so the above tests aren't exercising an unrelated bug.
func TestExtractAllManagedFields_FixtureSanity(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"spec": map[string]interface{}{
				"project": "default",
			},
		},
	}
	obj.SetManagedFields([]metav1.ManagedFieldsEntry{
		{
			Manager:   "k8sconnect",
			Operation: metav1.ManagedFieldsOperationApply,
			FieldsV1: &metav1.FieldsV1{
				Raw: buildFieldsV1(t, map[string]interface{}{
					"f:spec": map[string]interface{}{
						"f:project": map[string]interface{}{},
					},
				}),
			},
		},
	})

	got := fieldmanagement.ExtractAllManagedFields(obj)
	if _, ok := got["spec.project"]; !ok {
		t.Fatalf("fixture broken: ExtractAllManagedFields did not return spec.project; got %#v", got)
	}
}
