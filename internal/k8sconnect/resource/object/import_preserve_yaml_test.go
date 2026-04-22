package object_test

import (
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
// Reproduction path:
//  1. Apply a Service with minimal yaml_body (no clusterIP, type, sessionAffinity, etc.).
//     K8s admission adds defaults; k8sconnect owns only ports + selector.
//  2. Remove the Service from Terraform state (`tofu state rm`).
//  3. Import it back. Import serializes the full live object into state.yaml_body
//     (bloated: includes spec.clusterIP, spec.type, spec.sessionAffinity, ...).
//  4. Run plan. Dry-run of user's minimal yaml_body produces a projection whose
//     values equal state's stored projection (same user fields, same cluster values).
//  5. Pre-fix: checkDriftAndPreserveState sees matching projections and restores
//     plannedData.YAMLBody = stateData.YAMLBody (bloated). Apply SSA-sends the
//     bloated yaml with force=true. k8sconnect takes ownership of every field.
//     Post-apply projection balloons (e.g. 8 -> 16 entries). Framework trips
//     "Provider produced inconsistent result after apply: .managed_state_projection:
//     new element <X> has appeared" for each server-set default (spec.clusterIP,
//     spec.sessionAffinity, spec.ipFamilies, ...).
//
// Fix: preserve state yaml_body only when plannedData.YAMLBody also equals it.
// If user intent (yaml_body) differs, honor the new yaml_body.
func TestAccObjectResource_ImportPreservesUserYAMLBody(t *testing.T) {
	t.Parallel()

	raw := os.Getenv("TF_ACC_KUBECONFIG")
	if raw == "" {
		t.Fatal("TF_ACC_KUBECONFIG must be set")
	}

	ns := fmt.Sprintf("reimport-yaml-%d", time.Now().UnixNano()%1000000)
	svcName := fmt.Sprintf("reimport-svc-%d", time.Now().UnixNano()%1000000)
	k8sClient := testhelpers.CreateK8sClient(t, raw)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: map[string]func() (tfprotov6.ProviderServer, error){
			"k8sconnect": providerserver.NewProtocol6WithError(k8sconnect.New()),
		},
		Steps: []resource.TestStep{
			// Step 1: Apply user's minimal Service. Baseline.
			{
				Config: testAccReimportConfig(ns, svcName),
				ConfigVariables: config.Variables{
					"raw":  config.StringVariable(raw),
					"name": config.StringVariable(svcName),
				},
				Check: resource.ComposeTestCheckFunc(
					testhelpers.CheckServiceExists(k8sClient, ns, svcName),
				),
			},
			// Step 2: Re-import the Service. This makes state.yaml_body the full
			// live object (with clusterIP, type, etc.). Then re-apply with the
			// same user config. Pre-fix this triggered the inconsistency error.
			// ImportStatePersist + refresh + apply exercises the exact path.
			{
				Config: testAccReimportConfig(ns, svcName),
				ConfigVariables: config.Variables{
					"raw":  config.StringVariable(raw),
					"name": config.StringVariable(svcName),
				},
				ResourceName:       "k8sconnect_object.svc",
				ImportState:        true,
				ImportStatePersist: true,
				ImportStateId:      fmt.Sprintf("k3d-k8sconnect-test:%s:v1/Service:%s", ns, svcName),
				// Import yields a bloated yaml_body; ImportStateVerify would
				// complain about that against the user's minimal config. We
				// verify the behavior in the follow-up step instead.
				ImportStateVerify: false,
			},
			// Step 3: Plan + apply with the user's minimal config. This must not
			// trip "Provider produced inconsistent result". The projection and
			// managed_fields must stabilize on the scoped set (no server-set
			// defaults leaking in).
			{
				Config: testAccReimportConfig(ns, svcName),
				ConfigVariables: config.Variables{
					"raw":  config.StringVariable(raw),
					"name": config.StringVariable(svcName),
				},
				// A subsequent plan must show no changes: state has converged.
				Check: resource.ComposeTestCheckFunc(
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
					// managed_state_projection likewise scoped.
					resource.TestCheckNoResourceAttr("k8sconnect_object.svc",
						"managed_state_projection.spec.clusterIP"),
					resource.TestCheckNoResourceAttr("k8sconnect_object.svc",
						"managed_state_projection.spec.type"),
				),
			},
			// Step 4: Empty follow-up plan — state has converged.
			{
				Config: testAccReimportConfig(ns, svcName),
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

func testAccReimportConfig(namespace, svcName string) string {
	return fmt.Sprintf(`
variable "raw"  { type = string }
variable "name" { type = string }

provider "k8sconnect" {}

locals {
  cluster = { kubeconfig = var.raw }
}

resource "k8sconnect_object" "ns" {
  yaml_body = <<YAML
apiVersion: v1
kind: Namespace
metadata:
  name: %s
YAML
  cluster = local.cluster
}

resource "k8sconnect_object" "svc" {
  depends_on = [k8sconnect_object.ns]

  # Minimal Service yaml — no clusterIP, type, sessionAffinity, ipFamilies, etc.
  # K8s admission assigns all these defaults on the server side. The test
  # checks that those server-set defaults do NOT leak into state after a
  # re-import + apply cycle.
  yaml_body = <<YAML
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
  cluster = local.cluster
}
`, namespace, svcName, namespace, svcName)
}
