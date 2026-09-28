package wait

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func deployment(specReplicas *int64, generation int64, status map[string]interface{}) *unstructured.Unstructured {
	spec := map[string]interface{}{}
	if specReplicas != nil {
		spec["replicas"] = *specReplicas
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"generation": generation},
		"spec":     spec,
		"status":   status,
	}}
}

func int64p(v int64) *int64 { return &v }

func TestDeploymentRolloutComplete(t *testing.T) {
	cases := []struct {
		name string
		obj  *unstructured.Unstructured
		want bool
	}{
		{
			// The regression: maxSurge created the new pod (updated=1) while the old
			// one is still ready (ready=1). The old check passed here.
			name: "surge in progress, old pod still ready, new pod not yet available",
			obj: deployment(int64p(1), 2, map[string]interface{}{
				"observedGeneration": int64(2), "replicas": int64(2),
				"updatedReplicas": int64(1), "readyReplicas": int64(1), "availableReplicas": int64(1),
			}),
			want: false,
		},
		{
			name: "new pod available, old pod still terminating",
			obj: deployment(int64p(1), 2, map[string]interface{}{
				"observedGeneration": int64(2), "replicas": int64(2),
				"updatedReplicas": int64(1), "readyReplicas": int64(2), "availableReplicas": int64(2),
			}),
			want: false,
		},
		{
			name: "rollout complete",
			obj: deployment(int64p(1), 2, map[string]interface{}{
				"observedGeneration": int64(2), "replicas": int64(1),
				"updatedReplicas": int64(1), "readyReplicas": int64(1), "availableReplicas": int64(1),
			}),
			want: true,
		},
		{
			name: "controller has not observed the new generation",
			obj: deployment(int64p(1), 3, map[string]interface{}{
				"observedGeneration": int64(2), "replicas": int64(1),
				"updatedReplicas": int64(1), "readyReplicas": int64(1), "availableReplicas": int64(1),
			}),
			want: false,
		},
		{
			name: "updated replica not yet available",
			obj: deployment(int64p(3), 2, map[string]interface{}{
				"observedGeneration": int64(2), "replicas": int64(3),
				"updatedReplicas": int64(3), "readyReplicas": int64(2), "availableReplicas": int64(2),
			}),
			want: false,
		},
		{
			name: "scaled to zero is complete",
			obj: deployment(int64p(0), 2, map[string]interface{}{
				"observedGeneration": int64(2),
			}),
			want: true,
		},
		{
			name: "spec.replicas omitted defaults to one",
			obj: deployment(nil, 1, map[string]interface{}{
				"observedGeneration": int64(1), "replicas": int64(1),
				"updatedReplicas": int64(1), "readyReplicas": int64(1), "availableReplicas": int64(1),
			}),
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := deploymentRolloutComplete(tc.obj)
			if got != tc.want {
				t.Fatalf("deploymentRolloutComplete() = %v (%q), want %v", got, reason, tc.want)
			}
		})
	}
}
