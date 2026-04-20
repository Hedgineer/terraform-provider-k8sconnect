package fieldmanagement

import (
	"reflect"
	"testing"
)

func TestEncodePathKey(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"replicas", "replicas"},
		{"simple-key", "simple-key"},
		{"config.yaml", `"config.yaml"`},
		{"app.kubernetes.io/name", `"app.kubernetes.io/name"`},
		{"resource.customizations.ignoreDifferences.admissionregistration.k8s.io_ValidatingWebhookConfiguration",
			`"resource.customizations.ignoreDifferences.admissionregistration.k8s.io_ValidatingWebhookConfiguration"`},
		{`has"quote`, `"has\"quote"`},
		{`has\backslash`, `"has\\backslash"`},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := EncodePathKey(tt.in)
			if got != tt.want {
				t.Errorf("EncodePathKey(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	keys := []string{
		"replicas",
		"simple-key",
		"config.yaml",
		"app.kubernetes.io/name",
		"resource.customizations.ignoreDifferences.admissionregistration.k8s.io_ValidatingWebhookConfiguration",
		`has"quote`,
		`has\backslash`,
		`both."and\mixed`,
	}
	for _, k := range keys {
		t.Run(k, func(t *testing.T) {
			if got := DecodePathKey(EncodePathKey(k)); got != k {
				t.Errorf("round-trip %q -> %q -> %q", k, EncodePathKey(k), got)
			}
		})
	}
}

func TestSplitPath(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a.b.c", []string{"a", "b", "c"}},
		{`a."b.c".d`, []string{"a", `"b.c"`, "d"}},
		{`data."config.yaml"`, []string{"data", `"config.yaml"`}},
		{`metadata.annotations."app.kubernetes.io/name"`,
			[]string{"metadata", "annotations", `"app.kubernetes.io/name"`}},
		{"spec.containers[name=nginx].image",
			[]string{"spec", "containers[name=nginx]", "image"}},
		{"spec.containers[0].image",
			[]string{"spec", "containers[0]", "image"}},
		{`a."b\"c".d`, []string{"a", `"b\"c"`, "d"}},         // escaped quote inside quoted segment
		{`a."b.c[d].e".f`, []string{"a", `"b.c[d].e"`, "f"}}, // brackets inside quotes don't open selector
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := SplitPath(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SplitPath(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestFindSelectorStart(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"containers", -1},
		{"containers[name=nginx]", 10},
		{"containers[0]", 10},
		{`"has.brackets[x]"`, -1}, // brackets inside quotes don't count
		{`"key".foo`, -1},         // no selector; the `.foo` is out of this segment anyway
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := FindSelectorStart(tt.in)
			if got != tt.want {
				t.Errorf("FindSelectorStart(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}
