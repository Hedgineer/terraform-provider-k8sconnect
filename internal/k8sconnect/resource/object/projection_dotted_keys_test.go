package object

import (
	"reflect"
	"sort"
	"testing"
)

// TestExtractFieldPaths_QuotesDottedKeys is ADR-025's core contract check: when a
// map key contains '.', the emitted path wraps that key in quotes. ConfigMap data
// keys like the ArgoCD `resource.customizations.ignoreDifferences.*` family MUST
// become `data."resource.customizations..."`, not `data.resource.customizations...`.
// The latter format is ambiguous and caused the drift-detection collapse (see ADR-025).
func TestExtractFieldPaths_QuotesDottedKeys(t *testing.T) {
	obj := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      "argocd-cm",
			"namespace": "argocd",
			"labels": map[string]interface{}{
				"app.kubernetes.io/name": "argocd",
			},
		},
		"data": map[string]interface{}{
			"ui.bannercontent": "test",
			"ui.bannerpos":     "top",
			"resource.customizations.ignoreDifferences.admissionregistration.k8s.io_ValidatingWebhookConfiguration": "jqPathExpressions:\n  - .webhooks[]?.namespaceSelector\n",
			"resource.customizations.ignoreDifferences.gateway.networking.k8s.io_Gateway":                           "jqPathExpressions:\n  - .spec.listeners[]?.tls.certificateRefs[]?.group\n",
		},
	}

	got := extractFieldPaths(obj, "")
	sort.Strings(got)

	want := []string{
		"apiVersion",
		`data."resource.customizations.ignoreDifferences.admissionregistration.k8s.io_ValidatingWebhookConfiguration"`,
		`data."resource.customizations.ignoreDifferences.gateway.networking.k8s.io_Gateway"`,
		`data."ui.bannercontent"`,
		`data."ui.bannerpos"`,
		"kind",
		`metadata.labels."app.kubernetes.io/name"`,
		"metadata.name",
		"metadata.namespace",
	}
	sort.Strings(want)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("extractFieldPaths mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// TestProjectFields_RoundTripForDottedKeys verifies the full projection path
// (extract -> project -> flatten) preserves values for ConfigMap data keys that
// contain dots. Before ADR-025, projectFields' `getFieldByPath` dropped these
// silently, collapsing drift comparison to "no change" (the ArgoCD bug).
func TestProjectFields_RoundTripForDottedKeys(t *testing.T) {
	source := map[string]interface{}{
		"data": map[string]interface{}{
			"simple":                       "v1",
			"ui.banner":                    "v2",
			"app.kubernetes.io/annotation": "v3",
		},
	}

	paths := extractFieldPaths(source, "")

	projection, err := projectFields(source, paths)
	if err != nil {
		t.Fatalf("projectFields: %v", err)
	}

	flat := flattenProjectionToMap(projection, paths)

	want := map[string]string{
		"data.simple":                         "v1",
		`data."ui.banner"`:                    "v2",
		`data."app.kubernetes.io/annotation"`: "v3",
	}
	if !reflect.DeepEqual(flat, want) {
		t.Errorf("flattened projection mismatch\n got: %#v\nwant: %#v", flat, want)
	}
}

// TestProjectFields_DetectsDriftOnDottedKeys is the regression test for the exact
// ArgoCD ConfigMap patch scenario. Old state (cluster) has one dotted-key value;
// new desired state changes that value. Both projections must include the key
// (quote-encoded) and differ — if they collapse to equal, drift is invisible.
func TestProjectFields_DetectsDriftOnDottedKeys(t *testing.T) {
	stateObj := map[string]interface{}{
		"data": map[string]interface{}{
			"resource.customizations.ignoreDifferences.gateway.networking.k8s.io_Gateway": "old-value",
		},
	}
	desiredObj := map[string]interface{}{
		"data": map[string]interface{}{
			"resource.customizations.ignoreDifferences.gateway.networking.k8s.io_Gateway": "new-value",
		},
	}

	paths := extractFieldPaths(desiredObj, "")

	stateProj, _ := projectFields(stateObj, paths)
	desiredProj, _ := projectFields(desiredObj, paths)

	stateFlat := flattenProjectionToMap(stateProj, paths)
	desiredFlat := flattenProjectionToMap(desiredProj, paths)

	if reflect.DeepEqual(stateFlat, desiredFlat) {
		t.Fatalf("BUG: state and desired projections are equal, so drift is invisible.\n got: %#v", stateFlat)
	}

	key := `data."resource.customizations.ignoreDifferences.gateway.networking.k8s.io_Gateway"`
	if stateFlat[key] != "old-value" {
		t.Errorf("stateFlat[%q] = %q, want %q", key, stateFlat[key], "old-value")
	}
	if desiredFlat[key] != "new-value" {
		t.Errorf("desiredFlat[%q] = %q, want %q", key, desiredFlat[key], "new-value")
	}
}

// TestPathMatchesIgnorePattern_DottedKeyBackCompat verifies that user-provided
// ignore_fields patterns without quotes still match dotted-key paths — important
// so existing configs don't break. Explicit-quote form also matches.
func TestPathMatchesIgnorePattern_DottedKeyBackCompat(t *testing.T) {
	obj := map[string]interface{}{
		"data": map[string]interface{}{
			"config.yaml": "v",
		},
	}

	encodedPath := `data."config.yaml"`

	tests := []struct {
		name    string
		pattern string
		match   bool
	}{
		{"unescaped back-compat", "data.config.yaml", true},
		{"explicit quoted (authoritative)", `data."config.yaml"`, true},
		{"parent prefix matches", "data", true},
		{"unrelated key does not match", "data.other", false},
		{"too deep does not match", "data.config.yaml.extra", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pathMatchesIgnorePattern(encodedPath, tt.pattern, obj)
			if got != tt.match {
				t.Errorf("pathMatchesIgnorePattern(%q, %q) = %v, want %v",
					encodedPath, tt.pattern, got, tt.match)
			}
		})
	}
}
