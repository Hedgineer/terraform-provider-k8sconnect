package common

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// Kubernetes lists routinely mix element shapes: env entries with value vs
// valueFrom, tolerations with and without key, volumes of different sources.
// A Terraform list needs one element type, so these must convert to tuples.
func TestConvertToAttrValue_MixedShapeLists(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name  string
		input interface{}
		// navigate walks from the converted root to the list under test
		navigate func(t *testing.T, root attr.Value) attr.Value
		wantLen  int
	}{
		{
			name: "env value vs valueFrom",
			input: []interface{}{
				map[string]interface{}{"name": "PILOT_TRACE_SAMPLING", "value": "1"},
				map[string]interface{}{
					"name": "POD_NAME",
					"valueFrom": map[string]interface{}{
						"fieldRef": map[string]interface{}{"apiVersion": "v1", "fieldPath": "metadata.name"},
					},
				},
			},
			navigate: func(t *testing.T, root attr.Value) attr.Value { return root },
			wantLen:  2,
		},
		{
			name: "tolerations with and without key",
			input: []interface{}{
				map[string]interface{}{"operator": "Exists"},
				map[string]interface{}{"key": "CriticalAddonsOnly", "operator": "Exists"},
				map[string]interface{}{"effect": "NoExecute", "key": "node.kubernetes.io/not-ready", "operator": "Exists", "tolerationSeconds": int64(300)},
			},
			navigate: func(t *testing.T, root attr.Value) attr.Value { return root },
			wantLen:  3,
		},
		{
			name: "volumes of different sources",
			input: []interface{}{
				map[string]interface{}{"name": "local-certs", "emptyDir": map[string]interface{}{"medium": "Memory"}},
				map[string]interface{}{"name": "config-volume", "configMap": map[string]interface{}{"name": "istio", "defaultMode": int64(420)}},
				map[string]interface{}{"name": "cacerts", "secret": map[string]interface{}{"secretName": "cacerts", "optional": true}},
			},
			navigate: func(t *testing.T, root attr.Value) attr.Value { return root },
			wantLen:  3,
		},
		{
			name:     "int64 and float64 in one array",
			input:    []interface{}{int64(1), 2.5},
			navigate: func(t *testing.T, root attr.Value) attr.Value { return root },
			wantLen:  2,
		},
		{
			// containers differ only because their env lists differ in shape
			name: "mismatch nested below a list of objects",
			input: map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name": "a",
							"env":  []interface{}{map[string]interface{}{"name": "X", "value": "1"}},
						},
						map[string]interface{}{
							"name": "b",
							"env": []interface{}{map[string]interface{}{
								"name":      "Y",
								"valueFrom": map[string]interface{}{"secretKeyRef": map[string]interface{}{"name": "s", "key": "k"}},
							}},
						},
					},
				},
			},
			navigate: func(t *testing.T, root attr.Value) attr.Value {
				spec := root.(types.Object).Attributes()["spec"].(types.Object)
				return spec.Attributes()["containers"]
			},
			wantLen: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, err := ConvertToAttrValue(ctx, tc.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			tuple, ok := tc.navigate(t, root).(types.Tuple)
			if !ok {
				t.Fatalf("expected types.Tuple for mixed-shape list, got %T", tc.navigate(t, root))
			}
			if got := len(tuple.Elements()); got != tc.wantLen {
				t.Errorf("expected %d elements, got %d", tc.wantLen, got)
			}

			assertDynamicStateRoundTrip(t, root)
		})
	}
}

// Values are read back element by element, so check the content survives.
func TestConvertToAttrValue_MixedShapeListContent(t *testing.T) {
	ctx := context.Background()

	val, err := ConvertToAttrValue(ctx, []interface{}{
		map[string]interface{}{"name": "A", "value": "1"},
		map[string]interface{}{"name": "B", "valueFrom": map[string]interface{}{
			"fieldRef": map[string]interface{}{"fieldPath": "metadata.name"},
		}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	elems := val.(types.Tuple).Elements()
	first := elems[0].(types.Object).Attributes()
	if got := first["value"].(types.String).ValueString(); got != "1" {
		t.Errorf("expected env[0].value = 1, got %q", got)
	}
	second := elems[1].(types.Object).Attributes()
	fieldRef := second["valueFrom"].(types.Object).Attributes()["fieldRef"].(types.Object)
	if got := fieldRef.Attributes()["fieldPath"].(types.String).ValueString(); got != "metadata.name" {
		t.Errorf("expected env[1].valueFrom.fieldRef.fieldPath = metadata.name, got %q", got)
	}
}

// Lists whose elements share one type must stay types.List with the same
// element type. Existing state and user expressions depend on it.
func TestConvertToAttrValue_HomogeneousListsStayLists(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name         string
		input        []interface{}
		wantElemType attr.Type
	}{
		{
			name:         "strings",
			input:        []interface{}{"a", "b"},
			wantElemType: types.StringType,
		},
		{
			name:         "int64s",
			input:        []interface{}{int64(1), int64(2)},
			wantElemType: types.Int64Type,
		},
		{
			name: "objects with the same keys",
			input: []interface{}{
				map[string]interface{}{"name": "http", "port": int64(80)},
				map[string]interface{}{"name": "https", "port": int64(443)},
			},
			wantElemType: types.ObjectType{AttrTypes: map[string]attr.Type{
				"name": types.StringType,
				"port": types.Int64Type,
			}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			val, err := ConvertToAttrValue(ctx, tc.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			list, ok := val.(types.List)
			if !ok {
				t.Fatalf("expected types.List, got %T", val)
			}
			if !list.ElementType(ctx).Equal(tc.wantElemType) {
				t.Errorf("expected element type %s, got %s", tc.wantElemType, list.ElementType(ctx))
			}
			assertDynamicStateRoundTrip(t, val)
		})
	}
}

// assertDynamicStateRoundTrip stores the value in a dynamic attribute the way
// the object data source and wait resource do, then reads it back. A value
// that converts but cannot be stored would still fail for users at
// resp.State.Set.
func assertDynamicStateRoundTrip(t *testing.T, val attr.Value) {
	t.Helper()
	ctx := context.Background()

	s := schema.Schema{Attributes: map[string]schema.Attribute{
		"object": schema.DynamicAttribute{Computed: true},
	}}
	state := tfsdk.State{
		Schema: s,
		Raw:    tftypes.NewValue(s.Type().TerraformType(ctx), nil),
	}

	if diags := state.SetAttribute(ctx, path.Root("object"), types.DynamicValue(val)); diags.HasError() {
		t.Fatalf("failed to set dynamic state: %v", diags)
	}

	var got types.Dynamic
	if diags := state.GetAttribute(ctx, path.Root("object"), &got); diags.HasError() {
		t.Fatalf("failed to read dynamic state: %v", diags)
	}

	wantTF, err := val.ToTerraformValue(ctx)
	if err != nil {
		t.Fatalf("failed to convert expected value: %v", err)
	}
	gotTF, err := got.UnderlyingValue().ToTerraformValue(ctx)
	if err != nil {
		t.Fatalf("failed to convert stored value: %v", err)
	}
	if !wantTF.Equal(gotTF) {
		t.Errorf("state round trip changed the value:\nwant %s\ngot  %s", wantTF, gotTF)
	}
}
