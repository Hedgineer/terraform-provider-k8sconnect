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

func TestAccObjectDataSource_basic(t *testing.T) {
	t.Parallel()

	raw := os.Getenv("TF_ACC_KUBECONFIG")
	if raw == "" {
		t.Fatal("TF_ACC_KUBECONFIG must be set")
	}

	ns := fmt.Sprintf("ds-test-%d", time.Now().UnixNano()%1000000)
	cmName := fmt.Sprintf("config-%d", time.Now().UnixNano()%1000000)
	k8sClient := testhelpers.CreateK8sClient(t, raw)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: map[string]func() (tfprotov6.ProviderServer, error){
			"k8sconnect": providerserver.NewProtocol6WithError(k8sconnect.New()),
		},
		Steps: []resource.TestStep{
			// Step 1: Create resources and read with data source
			{
				Config: testAccObjectDataSourceConfig(ns, cmName),
				ConfigVariables: config.Variables{
					"raw":       config.StringVariable(raw),
					"namespace": config.StringVariable(ns),
					"name":      config.StringVariable(cmName),
				},
				Check: resource.ComposeTestCheckFunc(
					// Verify the object resource created successfully
					testhelpers.CheckConfigMapExists(k8sClient, ns, cmName),
					// Verify data source can read it
					resource.TestCheckResourceAttr("data.k8sconnect_object.test", "kind", "ConfigMap"),
					resource.TestCheckResourceAttr("data.k8sconnect_object.test", "api_version", "v1"),
					resource.TestCheckResourceAttr("data.k8sconnect_object.test", "name", cmName),
					resource.TestCheckResourceAttr("data.k8sconnect_object.test", "namespace", ns),
					// Verify outputs are populated
					resource.TestCheckResourceAttrSet("data.k8sconnect_object.test", "manifest"),
					resource.TestCheckResourceAttrSet("data.k8sconnect_object.test", "yaml_body"),
					// Verify we can access nested fields via .object attribute with dot notation
					resource.TestCheckOutput("test_data_key1", "value1"),
				),
			},
		},
	})
}

func testAccObjectDataSourceConfig(ns, name string) string {
	return fmt.Sprintf(`
variable "raw" {
  type = string
}
variable "namespace" {
  type = string
}
variable "name" {
  type = string
}

provider "k8sconnect" {}

resource "k8sconnect_object" "namespace" {
  yaml_body = <<YAML
apiVersion: v1
kind: Namespace
metadata:
  name: %s
YAML

  cluster = {
    kubeconfig = var.raw
  }
}

resource "k8sconnect_object" "test" {
  yaml_body = <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  key1: value1
  key2: value2
YAML

  cluster = {
    kubeconfig = var.raw
  }

  depends_on = [k8sconnect_object.namespace]
}

# Now read it with the data source
data "k8sconnect_object" "test" {
  api_version = "v1"
  kind        = "ConfigMap"
  name        = var.name
  namespace   = var.namespace

  cluster = {
    kubeconfig = var.raw
  }

  depends_on = [k8sconnect_object.test]
}

# Test that we can access nested fields via .object attribute
output "test_data_key1" {
  value = data.k8sconnect_object.test.object.data.key1
}
`, ns, name, ns)
}

// Kubernetes arrays often mix element shapes. Here the container env has one
// entry with value and one with valueFrom, which cannot share a list element
// type. Reading such an object used to fail with "Value Conversion Error".
func TestAccObjectDataSource_mixedShapeList(t *testing.T) {
	t.Parallel()

	raw := os.Getenv("TF_ACC_KUBECONFIG")
	if raw == "" {
		t.Fatal("TF_ACC_KUBECONFIG must be set")
	}

	ns := fmt.Sprintf("ds-mixed-%d", time.Now().UnixNano()%1000000)
	deployName := fmt.Sprintf("mixed-%d", time.Now().UnixNano()%1000000)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: map[string]func() (tfprotov6.ProviderServer, error){
			"k8sconnect": providerserver.NewProtocol6WithError(k8sconnect.New()),
		},
		Steps: []resource.TestStep{
			{
				Config: testAccObjectDataSourceMixedShapeConfig(ns, deployName),
				ConfigVariables: config.Variables{
					"raw":       config.StringVariable(raw),
					"namespace": config.StringVariable(ns),
					"name":      config.StringVariable(deployName),
				},
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("data.k8sconnect_object.test", "kind", "Deployment"),
					resource.TestCheckOutput("env_count", "2"),
					resource.TestCheckOutput("env0_value", "1"),
					resource.TestCheckOutput("env1_field_path", "metadata.name"),
				),
			},
		},
	})
}

func testAccObjectDataSourceMixedShapeConfig(ns, name string) string {
	return fmt.Sprintf(`
variable "raw" {
  type = string
}
variable "namespace" {
  type = string
}
variable "name" {
  type = string
}

provider "k8sconnect" {}

resource "k8sconnect_object" "namespace" {
  yaml_body = <<YAML
apiVersion: v1
kind: Namespace
metadata:
  name: %s
YAML

  cluster = {
    kubeconfig = var.raw
  }
}

# replicas: 0 so no pods are scheduled and no image is pulled
resource "k8sconnect_object" "test" {
  yaml_body = <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  replicas: 0
  selector:
    matchLabels:
      app: mixed
  template:
    metadata:
      labels:
        app: mixed
    spec:
      containers:
      - name: app
        image: registry.k8s.io/pause:3.9
        env:
        - name: PLAIN
          value: "1"
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
YAML

  cluster = {
    kubeconfig = var.raw
  }

  depends_on = [k8sconnect_object.namespace]
}

data "k8sconnect_object" "test" {
  api_version = "apps/v1"
  kind        = "Deployment"
  name        = var.name
  namespace   = var.namespace

  cluster = {
    kubeconfig = var.raw
  }

  depends_on = [k8sconnect_object.test]
}

locals {
  env = data.k8sconnect_object.test.object.spec.template.spec.containers[0].env
}

output "env_count" {
  value = tostring(length(local.env))
}

output "env0_value" {
  value = local.env[0].value
}

output "env1_field_path" {
  value = local.env[1].valueFrom.fieldRef.fieldPath
}
`, ns, name, ns)
}
