package integration

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/turfbuild/terraform-provider-kubewait/internal/fixtures"
	"github.com/turfbuild/terraform-provider-kubewait/internal/wait"
)

var (
	certGVR     = schema.GroupVersionResource{Group: "nvcre.nvidia.com", Version: "v1alpha1", Resource: "certifications"}
	trainJobGVR = schema.GroupVersionResource{Group: "trainer.kubeflow.org", Version: "v1alpha1", Resource: "trainjobs"}
)

// createCertification creates a Certification that the real v0.2.0 CRD
// accepts, with the spec shape modules/certification renders.
func createCertification(t *testing.T, ns, name string) {
	t.Helper()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "nvcre.nvidia.com/v1alpha1",
		"kind":       "Certification",
		"metadata":   map[string]any{"namespace": ns, "name": name},
		"spec": map[string]any{
			"target": map[string]any{"nodeNames": []any{"ip-10-0-130-4.ec2.internal", "ip-10-0-131-7.ec2.internal"}},
			"categories": []any{
				map[string]any{"domain": "communication", "variant": "nccl-all-reduce"},
				map[string]any{"domain": "compute", "variant": "gpu-burn"},
			},
		},
	}}
	if _, err := dyn.Resource(certGVR).Namespace(ns).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// cond is a metav1.Condition-shaped status entry (the CRD requires every
// field).
func cond(typ, status, reason string) map[string]any {
	return map[string]any{"type": typ, "status": status, "reason": reason, "message": reason,
		"lastTransitionTime": time.Now().UTC().Format(time.RFC3339)}
}

// setStatus replaces status through the status subresource, as the
// controller would.
func setStatus(t *testing.T, gvr schema.GroupVersionResource, ns, name string, status map[string]any) {
	t.Helper()
	retry(t, func() error {
		obj, err := dyn.Resource(gvr).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		obj.Object["status"] = status
		_, err = dyn.Resource(gvr).Namespace(ns).UpdateStatus(context.Background(), obj, metav1.UpdateOptions{})
		return err
	})
}

func certStatus(succeeded, failed, reason string, categories ...map[string]any) map[string]any {
	st := map[string]any{"conditions": []any{cond("Succeeded", succeeded, reason), cond("Failed", failed, reason)}}
	if len(categories) > 0 {
		var cs []any
		for _, c := range categories {
			cs = append(cs, c)
		}
		st["categoryStatuses"] = cs
	}
	return st
}

func category(domain, variant, status string) map[string]any {
	return map[string]any{"domain": domain, "variant": variant, "status": status}
}

// certificationTerminal is use 2 on a short clock.
func certificationTerminal(ns, settle, timeout string) wait.Raw {
	raw := fixtures.CertificationTerminal(ns, "gpu-pools", timeout, settle)
	raw.PollInterval = fixtures.S("1s")
	return raw
}

func TestCertificationSucceeded(t *testing.T) {
	requireEnv(t)
	ns := namespace(t, "cert-ok")
	createCertification(t, ns, "gpu-pools")
	setStatus(t, certGVR, ns, "gpu-pools", certStatus("False", "False", "WorkflowRunning",
		category("communication", "nccl-all-reduce", "InProgress"), category("compute", "gpu-burn", "Pending")))

	w := startWait(t, certificationTerminal(ns, "2s", "60s"), reader)
	w.awaitProgress("pending: "+ns+"/gpu-pools: Succeeded=False (WorkflowRunning)",
		"status.conditions=[Succeeded=False(WorkflowRunning) Failed=False(WorkflowRunning)]",
		`status.categoryStatuses=[{"domain":"communication","status":"InProgress","variant":"nccl-all-reduce"},{"domain":"compute","status":"Pending","variant":"gpu-burn"}]`)
	setStatus(t, certGVR, ns, "gpu-pools", certStatus("True", "False", "AllCategoriesPassed",
		category("communication", "nccl-all-reduce", "Succeeded"), category("compute", "gpu-burn", "Succeeded")))
	w.awaitProgress("success: " + ns + "/gpu-pools: Succeeded=True")
	w.expectSuccess()
}

func TestCertificationFailed(t *testing.T) {
	requireEnv(t)
	ns := namespace(t, "cert-fail")
	createCertification(t, ns, "gpu-pools")
	setStatus(t, certGVR, ns, "gpu-pools", certStatus("False", "False", "WorkflowRunning"))
	w := startWait(t, certificationTerminal(ns, "1s", "60s"), reader)
	w.awaitProgress("pending:")
	setStatus(t, certGVR, ns, "gpu-pools", certStatus("False", "True", "CategoryFailed",
		category("communication", "nccl-all-reduce", "Failed")))
	w.expectFailure("kubewait_condition failed", ns+"/gpu-pools: Failed=True (CategoryFailed)", "held for settle (1s)",
		`"status":"Failed"`)
}

// NVCRE with repeatCount > 1 can move Failed back to InProgress. A failure
// that does not survive settle does not end the wait.
func TestCertificationFailedFlipsBack(t *testing.T) {
	requireEnv(t)
	ns := namespace(t, "cert-flip")
	createCertification(t, ns, "gpu-pools")
	setStatus(t, certGVR, ns, "gpu-pools", certStatus("False", "False", "WorkflowRunning"))
	w := startWait(t, certificationTerminal(ns, "4s", "60s"), reader)
	w.awaitProgress("pending:")
	setStatus(t, certGVR, ns, "gpu-pools", certStatus("False", "True", "CategoryFailed"))
	w.awaitProgress("failure: " + ns + "/gpu-pools: Failed=True")
	time.Sleep(time.Second)
	setStatus(t, certGVR, ns, "gpu-pools", certStatus("False", "False", "WorkflowRestarted"))
	w.awaitProgress("pending: " + ns + "/gpu-pools: Succeeded=False (WorkflowRestarted)")
	w.still(5 * time.Second) // past where the failure would have settled
	setStatus(t, certGVR, ns, "gpu-pools", certStatus("True", "False", "AllCategoriesPassed"))
	w.expectSuccess()
}

func TestCertificationNeverCreated(t *testing.T) {
	requireEnv(t)
	ns := namespace(t, "cert-absent")
	w := startWait(t, certificationTerminal(ns, "0s", "60s"), reader)
	r := w.expectFailure("kubewait_condition failed", ns+"/gpu-pools not found (absent = failure)")
	if _, took := w.finish(); took > 5*time.Second {
		t.Errorf("absent = failure took %s", took)
	}
	_ = r
}
