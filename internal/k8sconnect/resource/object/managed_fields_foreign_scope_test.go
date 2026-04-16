package object_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/config"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/jmorris0x0/terraform-provider-k8sconnect/internal/k8sconnect"
	testhelpers "github.com/jmorris0x0/terraform-provider-k8sconnect/internal/k8sconnect/common/test"
)

// TestAccObjectResource_ForeignControllerOutsideScope reproduces the ArgoCD
// Application pattern that triggered "Provider produced inconsistent result
// after apply" errors: an external controller writes a spec-level field that
// is NOT in our yaml_body and that we have never owned.
//
// Before ADR-024 scoping, managed_fields tracked every field manager on the
// object — so the foreign path would show up in the post-apply managed_fields
// but not in the plan's prediction, causing Terraform's framework to fail the
// apply.
//
// With ADR-024 scoping, fields outside (yaml_body ∪ k8sconnect-owned ∪
// baseline-owned) are excluded from managed_fields, so the foreign path never
// enters the attribute and the apply succeeds.
//
// We use a ConfigMap (not a CRD) so the test has no additional dependencies:
// SSA tracks per-key ownership within `data`, so a foreign manager writing
// `data.external-key` is exactly analogous to ArgoCD's controller writing
// `operation.*` on an Application.
func TestAccObjectResource_ForeignControllerOutsideScope(t *testing.T) {
	t.Parallel()

	raw := os.Getenv("TF_ACC_KUBECONFIG")
	if raw == "" {
		t.Fatal("TF_ACC_KUBECONFIG must be set")
	}

	ns := fmt.Sprintf("foreign-scope-ns-%d", time.Now().UnixNano()%1000000)
	cmName := fmt.Sprintf("foreign-scope-cm-%d", time.Now().UnixNano()%1000000)
	k8sClient := testhelpers.CreateK8sClient(t, raw)
	ssaClient := testhelpers.NewSSATestClient(t, raw)

	cfg := fmt.Sprintf(`
variable "raw" { type = string }

resource "k8sconnect_object" "namespace" {
  yaml_body = <<YAML
apiVersion: v1
kind: Namespace
metadata:
  name: %s
YAML
  cluster = { kubeconfig = var.raw }
}

resource "k8sconnect_object" "cm" {
  depends_on = [k8sconnect_object.namespace]

  yaml_body = <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  key: value
YAML
  cluster = { kubeconfig = var.raw }
}
`, ns, cmName, ns)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: map[string]func() (tfprotov6.ProviderServer, error){
			"k8sconnect": providerserver.NewProtocol6WithError(k8sconnect.New()),
		},
		Steps: []resource.TestStep{
			// Step 1: Create ConfigMap with k8sconnect.
			{
				Config: cfg,
				ConfigVariables: config.Variables{
					"raw": config.StringVariable(raw),
				},
				Check: resource.ComposeTestCheckFunc(
					testhelpers.CheckConfigMapExists(k8sClient, ns, cmName),
					resource.TestCheckResourceAttr("k8sconnect_object.cm", "managed_fields.data.key", "k8sconnect"),
				),
			},
			// Step 2: A foreign controller SSA-writes a key that is NOT in our
			// yaml_body. This simulates the ArgoCD controller writing
			// operation.* on an Application. Then re-apply — which before
			// ADR-024 would fail with "Provider produced inconsistent result
			// after apply" because the foreign path appears in managed_fields
			// post-apply but not in the plan's prediction.
			{
				PreConfig: func() {
					ctx := context.Background()
					// Write a data key k8sconnect has never owned, using a
					// different field manager. The `force` flag doesn't matter
					// here because k8sconnect does not own this specific key.
					err := ssaClient.ApplyConfigMapDataSSA(ctx, ns, cmName,
						map[string]string{"external-key": "foreign-value"},
						"foreign-controller")
					if err != nil {
						t.Fatalf("SSA by foreign-controller failed: %v", err)
					}

					// Verify the cluster state actually has the foreign path
					// and a separate manager entry — otherwise the test isn't
					// exercising the scenario.
					cs := k8sClient.(*kubernetes.Clientset)
					cm, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, cmName, metav1.GetOptions{})
					if err != nil {
						t.Fatalf("get configmap: %v", err)
					}
					if _, ok := cm.Data["external-key"]; !ok {
						t.Fatalf("expected data.external-key on the cluster object; got data=%v", cm.Data)
					}
					sawForeign := false
					for _, mf := range cm.ManagedFields {
						if mf.Manager == "foreign-controller" {
							sawForeign = true
						}
					}
					if !sawForeign {
						t.Fatalf("expected foreign-controller in managedFields; got %+v", cm.ManagedFields)
					}
				},
				Config: cfg,
				ConfigVariables: config.Variables{
					"raw": config.StringVariable(raw),
				},
				// The critical assertions:
				// 1) Apply succeeds (no inconsistent-result error) — implicit
				//    in the step passing.
				// 2) managed_fields does NOT contain the foreign path.
				// 3) managed_fields still tracks our field correctly.
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("k8sconnect_object.cm", "managed_fields.data.key", "k8sconnect"),
					resource.TestCheckNoResourceAttr("k8sconnect_object.cm", "managed_fields.data.external-key"),
				),
			},
		},
		CheckDestroy: testhelpers.CheckNamespaceDestroy(k8sClient, ns),
	})
}
