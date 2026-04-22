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

	"github.com/jmorris0x0/terraform-provider-k8sconnect/internal/k8sconnect"
	testhelpers "github.com/jmorris0x0/terraform-provider-k8sconnect/internal/k8sconnect/common/test"
)

// TestAccObjectResource_ImportPreservesUserYAMLBody is the regression guard for
// the reimport-then-apply bug that broke client-jhc's staging deploys on
// hed-v0.3.8-p2. See ADR-026 for the full incident write-up.
//
// Mechanism:
//   - Import serializes the entire live Kubernetes object (including every
//     admission-defaulted field — clusterIP, sessionAffinity, ipFamilies,
//     ipFamilyPolicy, type, ports[0].protocol, ...) into state.yaml_body.
//   - On the next plan, ModifyPlan dry-runs the user's minimal yaml_body.
//     Projection values happen to match state's projection (same cluster state,
//     same user-owned fields).
//   - checkDriftAndPreserveState's pre-fix condition fired on matching
//     projections alone and restored stateData.YAMLBody (the bloated import)
//     into plannedData.YAMLBody.
//   - Apply SSA-sent the bloated yaml with force=true. k8sconnect took
//     ownership of every server-set default. Post-apply projection grew from
//     ~8 entries to ~16. Framework tripped "Provider produced inconsistent
//     result after apply: .managed_state_projection: new element <X>
//     has appeared" for each default field.
//
// Fix: only preserve when plannedData.YAMLBody also equals stateData.YAMLBody.
// If the user's yaml_body differs from state's (as it does post-import),
// skip preservation entirely — honor the user's config.
//
// Test construction: we create the Service with the k8sconnect field manager
// out-of-band (so the import won't complain about an unmanaged resource), then
// use Terraform's native `import {}` block to bring it into state along with
// a MINIMAL user yaml_body. Pre-fix the apply phase would fail; post-fix it
// converges on the scoped (ports+selector) state.
func TestAccObjectResource_ImportPreservesUserYAMLBody(t *testing.T) {
	t.Parallel()

	raw := os.Getenv("TF_ACC_KUBECONFIG")
	if raw == "" {
		t.Fatal("TF_ACC_KUBECONFIG must be set")
	}

	ns := fmt.Sprintf("reimport-yaml-%d", time.Now().UnixNano()%1000000)
	svcName := fmt.Sprintf("reimport-svc-%d", time.Now().UnixNano()%1000000)
	k8sClient := testhelpers.CreateK8sClient(t, raw)
	ssaClient := testhelpers.NewSSATestClient(t, raw)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: map[string]func() (tfprotov6.ProviderServer, error){
			"k8sconnect": providerserver.NewProtocol6WithError(k8sconnect.New()),
		},
		Steps: []resource.TestStep{
			// Step 1: create namespace via Terraform.
			{
				Config: testAccReimportNamespaceOnly(ns),
				ConfigVariables: config.Variables{
					"raw": config.StringVariable(raw),
				},
				Check: testhelpers.CheckNamespaceExists(k8sClient, ns),
			},
			// Step 2: Out-of-band create the Service with the k8sconnect field
			// manager (so a subsequent import succeeds), then import it via a
			// Terraform `import {}` block against a MINIMAL user yaml_body.
			//
			// This exactly reproduces the client-jhc flow: state gets a bloated
			// yaml_body from import, user's config has a minimal yaml_body,
			// apply has to reconcile. Pre-fix this step fails with "Provider
			// produced inconsistent result after apply"; post-fix it passes.
			{
				PreConfig: func() {
					ctx := context.Background()
					if err := ssaClient.ApplyMinimalServiceSSA(ctx, ns, svcName, "k8sconnect"); err != nil {
						t.Fatalf("failed to seed Service with SSA: %v", err)
					}
				},
				Config: testAccReimportWithImportBlock(ns, svcName),
				ConfigVariables: config.Variables{
					"raw":  config.StringVariable(raw),
					"name": config.StringVariable(svcName),
				},
				Check: resource.ComposeTestCheckFunc(
					testhelpers.CheckServiceExists(k8sClient, ns, svcName),
					// managed_fields only tracks k8sconnect-owned user paths.
					resource.TestCheckResourceAttr("k8sconnect_object.svc",
						"managed_fields.spec.selector", "k8sconnect"),
					// Server-set defaults must NOT appear in managed_fields.
					resource.TestCheckNoResourceAttr("k8sconnect_object.svc",
						"managed_fields.spec.clusterIP"),
					resource.TestCheckNoResourceAttr("k8sconnect_object.svc",
						"managed_fields.spec.sessionAffinity"),
					resource.TestCheckNoResourceAttr("k8sconnect_object.svc",
						"managed_fields.spec.ipFamilies"),
					resource.TestCheckNoResourceAttr("k8sconnect_object.svc",
						"managed_fields.spec.type"),
					// Projection scoped the same way.
					resource.TestCheckNoResourceAttr("k8sconnect_object.svc",
						"managed_state_projection.spec.clusterIP"),
					resource.TestCheckNoResourceAttr("k8sconnect_object.svc",
						"managed_state_projection.spec.type"),
				),
			},
			// Step 3: plan is empty — state has converged.
			{
				Config: testAccReimportWithImportBlock(ns, svcName),
				ConfigVariables: config.Variables{
					"raw":  config.StringVariable(raw),
					"name": config.StringVariable(svcName),
				},
				PlanOnly: true,
			},
		},
		CheckDestroy: testhelpers.CheckNamespaceDestroy(k8sClient, ns),
	})
}

func testAccReimportNamespaceOnly(namespace string) string {
	return fmt.Sprintf(`
variable "raw" { type = string }

provider "k8sconnect" {}

resource "k8sconnect_object" "ns" {
  yaml_body = <<-YAML
    apiVersion: v1
    kind: Namespace
    metadata:
      name: %s
  YAML
  cluster = { kubeconfig = var.raw }
}
`, namespace)
}

func testAccReimportWithImportBlock(namespace, svcName string) string {
	return fmt.Sprintf(`
variable "raw"  { type = string }
variable "name" { type = string }

provider "k8sconnect" {}

resource "k8sconnect_object" "ns" {
  yaml_body = <<-YAML
    apiVersion: v1
    kind: Namespace
    metadata:
      name: %s
  YAML
  cluster = { kubeconfig = var.raw }
}

# Import the existing Service. Pre-fix, the post-import apply cycle would
# fail because the preservation heuristic restored the bloated imported
# yaml_body into the plan.
import {
  to = k8sconnect_object.svc
  id = "k3d-k8sconnect-test:%s:v1/Service:%s"
}

resource "k8sconnect_object" "svc" {
  depends_on = [k8sconnect_object.ns]

  # Minimal user config — no clusterIP, type, sessionAffinity, ipFamilies, etc.
  yaml_body = <<-YAML
    apiVersion: v1
    kind: Service
    metadata:
      name: %s
      namespace: %s
    spec:
      ports:
        - name: tcp
          port: 6379
          targetPort: 6379
      selector:
        app.kubernetes.io/name: %s
  YAML
  cluster = { kubeconfig = var.raw }
}
`, namespace, namespace, svcName, svcName, namespace, svcName)
}
