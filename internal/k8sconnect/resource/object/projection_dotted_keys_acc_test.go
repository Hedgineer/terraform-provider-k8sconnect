package object_test

import (
	"fmt"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/config"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/jmorris0x0/terraform-provider-k8sconnect/internal/k8sconnect"
	testhelpers "github.com/jmorris0x0/terraform-provider-k8sconnect/internal/k8sconnect/common/test"
)

// TestAccObjectResource_DottedKeysInConfigMapData is the regression test for
// ADR-025 (path encoding). Reproduces the ArgoCD `argocd-cm` patch pattern:
// ConfigMap data keys contain dots (e.g. `resource.customizations.ignoreDifferences.*`,
// `ui.bannercontent`). Before the fix, projectFields' path-splitter treated the
// dots as nesting separators and silently dropped the entries, collapsing drift
// comparison to "no change" — the exact bug the two staging clients hit.
//
// This test creates a ConfigMap whose data keys match the real-world pattern,
// then modifies one dotted-key value and expects the plan to show a diff. If
// projection ever regresses to the old silent-skip behavior, step 2 will fail
// with "unexpected plan empty" because Terraform preserves yaml_body.
func TestAccObjectResource_DottedKeysInConfigMapData(t *testing.T) {
	t.Parallel()

	raw := os.Getenv("TF_ACC_KUBECONFIG")
	if raw == "" {
		t.Fatal("TF_ACC_KUBECONFIG must be set")
	}

	ns := fmt.Sprintf("dotted-keys-ns-%d", time.Now().UnixNano()%1000000)
	cmName := fmt.Sprintf("dotted-keys-cm-%d", time.Now().UnixNano()%1000000)
	k8sClient := testhelpers.CreateK8sClient(t, raw)

	// Mirrors the real argocd_cm_patch shape the user had in production.
	cfg := func(gatewayValue string) string {
		return fmt.Sprintf(`
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

resource "k8sconnect_object" "argocd_cm" {
  depends_on = [k8sconnect_object.namespace]

  yaml_body = <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  ui.bannercontent: "test-cluster"
  ui.bannerpos: "top"
  resource.customizations.ignoreDifferences.admissionregistration.k8s.io_ValidatingWebhookConfiguration: |
    jqPathExpressions:
      - .webhooks[]?.namespaceSelector
  resource.customizations.ignoreDifferences.gateway.networking.k8s.io_Gateway: |
    %s
YAML
  cluster = { kubeconfig = var.raw }
}
`, ns, cmName, ns, gatewayValue)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: map[string]func() (tfprotov6.ProviderServer, error){
			"k8sconnect": providerserver.NewProtocol6WithError(k8sconnect.New()),
		},
		Steps: []resource.TestStep{
			// Step 1: initial apply, all dotted-key values present.
			{
				Config: cfg(`jqPathExpressions:\n      - .spec.listeners[]?.tls.certificateRefs[]?.group`),
				ConfigVariables: config.Variables{
					"raw": config.StringVariable(raw),
				},
				Check: resource.ComposeTestCheckFunc(
					testhelpers.CheckConfigMapExists(k8sClient, ns, cmName),
					// The managed_fields map should list the dotted keys in quote-encoded form.
					resource.TestCheckResourceAttr(
						"k8sconnect_object.argocd_cm",
						`managed_fields.data."ui.bannercontent"`,
						"k8sconnect",
					),
					resource.TestCheckResourceAttr(
						"k8sconnect_object.argocd_cm",
						`managed_fields.data."resource.customizations.ignoreDifferences.gateway.networking.k8s.io_Gateway"`,
						"k8sconnect",
					),
					// And the projection keys too.
					resource.TestMatchResourceAttr(
						"k8sconnect_object.argocd_cm",
						`managed_state_projection.data."resource.customizations.ignoreDifferences.gateway.networking.k8s.io_Gateway"`,
						regexp.MustCompile("jqPathExpressions"),
					),
				),
			},
			// Step 2: change the value of ONE dotted key. Plan MUST show a diff.
			// Pre-fix this showed "no changes" because projectFields dropped the key.
			{
				Config: cfg(`jqPathExpressions:\n      - .spec.listeners[]?.tls.certificateRefs[]?.group\n      - .metadata.generation`),
				ConfigVariables: config.Variables{
					"raw": config.StringVariable(raw),
				},
				ExpectNonEmptyPlan: false, // after apply, plan should settle to empty
				Check: resource.ComposeTestCheckFunc(
					resource.TestMatchResourceAttr(
						"k8sconnect_object.argocd_cm",
						`managed_state_projection.data."resource.customizations.ignoreDifferences.gateway.networking.k8s.io_Gateway"`,
						regexp.MustCompile("metadata.generation"),
					),
				),
			},
		},
		CheckDestroy: testhelpers.CheckNamespaceDestroy(k8sClient, ns),
	})
}
