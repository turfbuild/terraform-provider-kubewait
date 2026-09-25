package acceptance

import (
	"context"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/config"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// certificationHook is use 2's shape on a short clock: a terraform_data
// stands in for the kubernetes_manifest a real configuration attaches it to.
func certificationHook(ns, onFailure string) string {
	of := ""
	if onFailure != "" {
		of = "\n      on_failure = " + onFailure
	}
	return hcl(fmt.Sprintf(`
resource "terraform_data" "certification" {
  input = "gpu-pools"
  lifecycle {
    action_trigger {
      events     = [after_create]
      actions    = [action.kubewait_condition.certification_terminal]%s
    }
  }
}

action "kubewait_condition" "certification_terminal" {
  config {
    api_version        = "nvcre.nvidia.com/v1alpha1"
    kind               = "Certification"
    namespace          = %q
    name               = "gpu-pools"
    success_conditions = [{ type = "Succeeded", status = "True" }]
    failure_conditions = [{ type = "Failed", status = "True" }]
    absent             = "failure"
    timeout            = "60s"
    settle             = "1s"
    poll_interval      = "1s"
    progress_fields    = ["status.conditions"]
  }
}
`, of, ns))
}

var hookInvocation = expectInvocation{
	action:   "action.kubewait_condition.certification_terminal",
	resource: "terraform_data.certification",
	event:    "after_create",
}

// after_create holds the resource's apply until the Certification succeeds.
func TestAccAfterCreateWaitsForSuccess(t *testing.T) {
	requireEnv(t)
	ns := namespace(t)
	createCertification(t, ns)
	setCertification(t, ns, "False", "False", "WorkflowRunning")
	var start time.Time
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   terraformChecks,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			PreConfig: func() {
				start = time.Now()
				later(4*time.Second, func() { setCertification(t, ns, "True", "False", "AllCategoriesPassed") })
			},
			Config:           certificationHook(ns, ""),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{hookInvocation}},
			Check: resource.ComposeTestCheckFunc(
				func(*terraform.State) error { return elapsedAtLeast(&start, 4*time.Second)() },
				resource.TestCheckResourceAttr("terraform_data.certification", "input", "gpu-pools"),
			),
		}},
	})
}

// A Failed Certification fails the apply (on_failure defaults to halt).
func TestAccAfterCreateFailureHalts(t *testing.T) {
	requireEnv(t)
	ns := namespace(t)
	createCertification(t, ns)
	setCertification(t, ns, "False", "False", "WorkflowRunning")
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   terraformChecks,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			PreConfig:   func() { later(2*time.Second, func() { setCertification(t, ns, "False", "True", "CategoryFailed") }) },
			Config:      certificationHook(ns, ""),
			ExpectError: regexp.MustCompile(`(?s)kubewait_condition failed.*` + ns + `/gpu-pools: Failed=True \(CategoryFailed\)`),
		}},
	})
}

// on_failure = continue: the failure is a warning and the apply succeeds.
func TestAccAfterCreateContinue(t *testing.T) {
	requireEnv(t)
	ns := namespace(t)
	createCertification(t, ns)
	setCertification(t, ns, "False", "True", "CategoryFailed")
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   terraformChecks,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config:           certificationHook(ns, "continue"),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{hookInvocation}},
			Check:            resource.TestCheckResourceAttr("terraform_data.certification", "input", "gpu-pools"),
		}},
	})
}

// on_failure = taint (use 2's setting): the apply fails and taints the
// resource; the next apply replaces it and waits again.
func TestAccAfterCreateTaint(t *testing.T) {
	requireEnv(t)
	ns := namespace(t)
	createCertification(t, ns)
	setCertification(t, ns, "False", "True", "CategoryFailed")
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   terraformChecks,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config:      certificationHook(ns, "taint"),
				ExpectError: regexp.MustCompile(`kubewait_condition failed`),
			},
			{
				PreConfig: func() { setCertification(t, ns, "True", "False", "AllCategoriesPassed") },
				Config:    certificationHook(ns, "taint"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction("terraform_data.certification", plancheck.ResourceActionReplace),
					hookInvocation,
				}},
			},
		},
	})
}

// drainGates is uses 3 and 5's shape: a resource whose destroy is gated
// before (a namespace must hold no gate=before pods) and confirmed after
// (no gate=after pods). triggers_replace drives the destroy: under
// `terraform destroy` Terraform never lets an action block (below).
func drainGates(ns, generation, afterTimeout string) string {
	return hcl(fmt.Sprintf(`
resource "terraform_data" "contents" {
  input            = "x"
  triggers_replace = %q
  lifecycle {
    action_trigger {
      events  = [before_destroy]
      actions = [action.kubewait_condition.nothing_left_before]
    }
    action_trigger {
      events  = [after_destroy]
      actions = [action.kubewait_condition.nothing_left_after]
    }
  }
}

action "kubewait_condition" "nothing_left_before" {
  config {
    api_version     = "v1"
    kind            = "Pod"
    namespace       = %q
    label_selector  = "gate=before"
    min_matching    = 0
    max_matching    = 0
    timeout         = "4s"
    progress_fields = ["metadata.name"]
  }
}

action "kubewait_condition" "nothing_left_after" {
  config {
    api_version     = "v1"
    kind            = "Pod"
    namespace       = %q
    label_selector  = "gate=after"
    min_matching    = 0
    max_matching    = 0
    timeout         = %q
    progress_fields = ["metadata.name"]
  }
}
`, generation, ns, ns, afterTimeout))
}

func labelPod(t *testing.T, ns, name, gate string) {
	t.Helper()
	createPod(t, ns, name)
	p, err := kube.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p.Labels = map[string]string{"gate": gate}
	if _, err := kube.CoreV1().Pods(ns).Update(context.Background(), p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// before_destroy as a gate: with a pod left behind, the replace halts
// before the delete and the old object survives; once it is gone, the
// replace goes through.
func TestAccBeforeDestroyGate(t *testing.T) {
	requireEnv(t)
	ns := namespace(t)
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   terraformChecks,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{Config: drainGates(ns, "1", "30s")},
			{
				PreConfig: func() { labelPod(t, ns, "blocker", "before") },
				Config:    drainGates(ns, "2", "30s"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					expectInvocation{"action.kubewait_condition.nothing_left_before", "terraform_data.contents", "before_destroy"},
				}},
				ExpectError: regexp.MustCompile(`(?s)kubewait_condition timed out.*1 still match: ` + ns + `/blocker`),
			},
			{
				// The old object survived the halted replace.
				PlanOnly:           true,
				Config:             drainGates(ns, "2", "30s"),
				ExpectNonEmptyPlan: true,
			},
			{
				PreConfig: func() { deletePod(t, ns, "blocker") },
				Config:    drainGates(ns, "2", "30s"),
				Check:     resource.TestCheckResourceAttr("terraform_data.contents", "triggers_replace", "2"),
			},
		},
	})
}

// after_destroy as a confirmation: the replace waits until the pods the
// delete should have removed are gone.
func TestAccAfterDestroyDrain(t *testing.T) {
	requireEnv(t)
	ns := namespace(t)
	var start time.Time
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   terraformChecks,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{Config: drainGates(ns, "1", "60s")},
			{
				PreConfig: func() {
					labelPod(t, ns, "worker-0", "after")
					labelPod(t, ns, "worker-1", "after")
					start = time.Now()
					later(4*time.Second, func() {
						deletePod(t, ns, "worker-0")
						deletePod(t, ns, "worker-1")
					})
				},
				Config: drainGates(ns, "2", "60s"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					expectInvocation{"action.kubewait_condition.nothing_left_after", "terraform_data.contents", "after_destroy"},
				}},
				Check: func(*terraform.State) error { return elapsedAtLeast(&start, 4*time.Second)() },
			},
		},
	})
}

// Measured, and pinned here so a Terraform change is noticed: a full
// `terraform destroy` never lets an action block. Terraform 1.16.2
// (internal/terraform/node_resource_abstract_instance.go, invokeDestroyActions:
// "a full destroy walk must never be blocked") downgrades destroy-event
// action failures to warnings whatever on_failure says, so the gate warns
// and the delete proceeds.
func TestAccDestroyModeNeverBlocks(t *testing.T) {
	requireEnv(t)
	ns := namespace(t)
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   terraformChecks,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{Config: drainGates(ns, "1", "2s")},
			{
				PreConfig: func() {
					labelPod(t, ns, "blocker", "before")
					labelPod(t, ns, "straggler", "after")
				},
				Config:  drainGates(ns, "1", "2s"),
				Destroy: true,
			},
		},
		CheckDestroy: func(*terraform.State) error {
			// The waits only observe: both pods are still there.
			for _, p := range []string{"blocker", "straggler"} {
				if _, err := kube.CoreV1().Pods(ns).Get(context.Background(), p, metav1.GetOptions{}); err != nil {
					return fmt.Errorf("%s: %w", p, err)
				}
			}
			return nil
		},
	})
}

// validation is a minimal set-mode action with an overridable body.
func validation(body string) string {
	return hcl(`
resource "terraform_data" "r" {
  lifecycle {
    action_trigger {
      events  = [after_create]
      actions = [action.kubewait_condition.w]
    }
  }
}

variable "settle" {
  type    = string
  default = "0s"
}

action "kubewait_condition" "w" {
  config {
    api_version = "v1"
    kind        = "Pod"
    namespace   = "default"
` + body + `
  }
}
`)
}

// Each of the spec's five rejections fails the plan, and so does one that
// arrives through a variable, which ValidateActionConfig sees as unknown:
// PlanAction (ModifyPlan) catches it.
func TestAccPlanTimeRejections(t *testing.T) {
	requireEnv(t)
	cases := []struct {
		name string
		body string
		vars config.Variables
		err  string
	}{
		{"name with a selector", `name = "p"
    label_selector = "app=x"
    timeout = "1m"`, nil, `name selects a single object`},
		{"min above max", `min_matching = 3
    max_matching = 2
    timeout = "1m"`, nil, `min_matching \(3\) is greater than max_matching \(2\)`},
		{"settle not below timeout", `timeout = "1m"
    settle = "1m"`, nil, `settle \(1m\) must be shorter than timeout \(1m\)`},
		{"bad CEL", `filter = "object.spec.type =="
    timeout = "1m"`, nil, `Invalid CEL in filter`},
		{"drain with success predicates", `min_matching = 0
    max_matching = 0
    success_conditions = [{ type = "Ready", status = "True" }]
    timeout = "1m"`, nil, `Drain with success predicates`},
		{"violation through a variable", `timeout = "1m"
    settle = var.settle`, config.Variables{"settle": config.StringVariable("5m")}, `settle \(5m\) must be shorter than timeout \(1m\)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				TerraformVersionChecks:   terraformChecks,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{{
					Config:          validation(tc.body),
					ConfigVariables: tc.vars,
					PlanOnly:        true,
					ExpectError:     regexp.MustCompile(tc.err),
				}},
			})
		})
	}
}
