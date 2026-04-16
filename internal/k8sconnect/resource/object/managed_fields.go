package object

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/jmorris0x0/terraform-provider-k8sconnect/internal/k8sconnect/common/fieldmanagement"
)

// Type alias for compatibility
type ManagedFields = fieldmanagement.ManagedFields

// updateManagedFieldsData updates the managed_fields attribute in the resource model
// for fields that are in scope: paths currently owned by k8sconnect, paths previously
// owned by k8sconnect (from the ADR-021 baseline), and paths present in yaml_body.
// Fields written by other controllers outside this scope are excluded to prevent
// "Provider produced inconsistent result after apply" errors when external controllers
// write unrelated fields between plan and apply (see ADR-024).
//
// Status fields and K8s system annotations are filtered within the scope as well
// (they are excluded regardless of ownership because they change unpredictably).
func updateManagedFieldsData(ctx context.Context, data *objectResourceModel, currentObj *unstructured.Unstructured, yamlBody string, baseline map[string]string) {
	// Extract ALL field ownership (map[string][]string)
	ownership := fieldmanagement.ExtractAllManagedFields(currentObj)

	// Compute the set of paths we consider relevant for managed_fields
	relevantPaths := computeRelevantManagedFieldPaths(yamlBody, baseline, ownership)

	// Filter ownership down to the relevant scope, applying status/system-annotation filters
	filteredOwnership := make(map[string][]string)
	for path, managers := range ownership {
		if !relevantPaths[path] {
			continue
		}

		// Skip status fields - they're always owned by controllers and provide no actionable information
		if strings.HasPrefix(path, "status.") || path == "status" {
			continue
		}

		// Skip K8s system annotations that are added/updated unpredictably by controllers
		// These cause plan/apply inconsistencies because they appear after our dry-run prediction
		if fieldmanagement.IsKubernetesSystemAnnotation(path) {
			continue
		}

		filteredOwnership[path] = managers
	}

	// Flatten using the common logic
	ownershipMap := fieldmanagement.FlattenManagedFields(filteredOwnership)

	// Convert to types.Map
	mapValue, diags := types.MapValueFrom(ctx, types.StringType, ownershipMap)
	if diags.HasError() {
		tflog.Warn(ctx, "Failed to convert field ownership to map", map[string]interface{}{
			"diagnostics": diags,
		})
		// Set empty map on error
		emptyMap, _ := types.MapValueFrom(ctx, types.StringType, map[string]string{})
		data.ManagedFields = emptyMap
	} else {
		data.ManagedFields = mapValue
	}
}

// computeRelevantManagedFieldPaths returns the union of paths considered in scope for
// managed_fields tracking (ADR-024):
//  1. Paths currently owned by k8sconnect (from the live ownership map)
//  2. Paths previously owned by k8sconnect (from the ownership_baseline in private state)
//  3. Paths referenced in the user's yaml_body
//
// Paths outside this union are treated as controller internals that k8sconnect has no
// relationship with — tracking them produces noise and spurious inconsistency errors.
func computeRelevantManagedFieldPaths(yamlBody string, baseline map[string]string, ownership map[string][]string) map[string]bool {
	relevant := make(map[string]bool)

	// (1) Currently owned by k8sconnect
	for path, managers := range ownership {
		for _, m := range managers {
			if m == "k8sconnect" {
				relevant[path] = true
				break
			}
		}
	}

	// (2) Previously owned by k8sconnect (ADR-021 baseline)
	for path, manager := range baseline {
		if manager == "k8sconnect" {
			relevant[path] = true
		}
	}

	// (3) Paths referenced in yaml_body
	if yamlBody != "" {
		obj := &unstructured.Unstructured{}
		if err := sigsyaml.Unmarshal([]byte(yamlBody), obj); err == nil {
			for _, p := range extractAllFieldsFromYAML(obj.Object, "") {
				relevant[p] = true
			}
		}
	}

	return relevant
}

// readOwnershipBaseline deserializes the ownership_baseline from private state.
// Returns nil if the key is missing or unparseable.
func readOwnershipBaseline(ctx context.Context, getter interface {
	GetKey(context.Context, string) ([]byte, diag.Diagnostics)
}) map[string]string {
	raw, diags := getter.GetKey(ctx, "ownership_baseline")
	if diags.HasError() || raw == nil {
		return nil
	}
	var baseline map[string]string
	if err := json.Unmarshal(raw, &baseline); err != nil {
		tflog.Debug(ctx, "Failed to parse ownership baseline from private state", map[string]interface{}{
			"error": err.Error(),
		})
		return nil
	}
	return baseline
}

// saveOwnershipBaseline extracts ownership information from a K8s object
// and saves it to private state as a JSON-serialized baseline for drift detection (ADR-021).
// This baseline represents "what we owned at last Apply" and is NOT updated during Read operations.
func saveOwnershipBaseline(ctx context.Context, privateState interface {
	SetKey(context.Context, string, []byte) diag.Diagnostics
}, obj *unstructured.Unstructured, ignoreFields []string) {
	// Extract ALL field ownership (map[string][]string)
	ownership := fieldmanagement.ExtractAllManagedFields(obj)

	// Flatten to map[string]string (first manager only, for simplicity)
	// This is sufficient for drift detection - we just need to know who owned what
	// IMPORTANT: Filter out ignored fields before saving to baseline (ADR-021 fix)
	baselineOwnership := make(map[string]string)
	for path, managers := range ownership {
		// Skip fields that are currently in ignore_fields
		if stringSliceContains(ignoreFields, path) {
			continue
		}
		if len(managers) > 0 {
			baselineOwnership[path] = managers[0] // Take first manager
		}
	}

	// Serialize to JSON
	baselineJSON, err := json.Marshal(baselineOwnership)
	if err != nil {
		tflog.Warn(ctx, "Failed to serialize ownership baseline", map[string]interface{}{
			"error": err.Error(),
		})
		return
	}

	// Save to private state
	diags := privateState.SetKey(ctx, "ownership_baseline", baselineJSON)
	if diags.HasError() {
		tflog.Warn(ctx, "Failed to save ownership baseline to private state", map[string]interface{}{
			"diagnostics": diags,
		})
		return
	}

	tflog.Debug(ctx, "Saved ownership baseline to private state", map[string]interface{}{
		"field_count": len(baselineOwnership),
	})
}
