package integration

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/turfbuild/terraform-provider-kubewait/internal/fixtures"
	"github.com/turfbuild/terraform-provider-kubewait/internal/wait"
)

func createTrainJob(t *testing.T, ns, name string) {
	t.Helper()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "trainer.kubeflow.org/v1alpha1",
		"kind":       "TrainJob",
		"metadata":   map[string]any{"namespace": ns, "name": name},
		"spec":       map[string]any{"runtimeRef": map[string]any{"name": "torch-distributed"}},
	}}
	if _, err := dyn.Resource(trainJobGVR).Namespace(ns).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func deleteTrainJob(t *testing.T, ns, name string) {
	t.Helper()
	if err := dyn.Resource(trainJobGVR).Namespace(ns).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
}

func trainJobFinished(ns, timeout string) wait.Raw {
	raw := fixtures.TrainJobFinished(ns, "pytorch-mnist", timeout)
	raw.PollInterval = fixtures.S("1s")
	return raw
}

// Use 4: a single TrainJob to Complete, or Failed.
func TestTrainJobFinished(t *testing.T) {
	requireEnv(t)
	for _, tc := range []struct {
		name, condition, reason string
		success                 bool
	}{
		{"complete", "Complete", "JobsCompleted", true},
		{"failed", "Failed", "TrainingFailed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := namespace(t, "tj-"+tc.name)
			createTrainJob(t, ns, "pytorch-mnist")
			w := startWait(t, trainJobFinished(ns, "60s"), reader)
			w.awaitProgress("pending: "+ns+"/pytorch-mnist: Complete missing", "status.conditions=<none>")
			setStatus(t, trainJobGVR, ns, "pytorch-mnist", map[string]any{"conditions": []any{cond(tc.condition, "True", tc.reason)}})
			if tc.success {
				w.expectSuccess()
			} else {
				w.expectFailure("kubewait_condition failed", "Failed=True (TrainingFailed)")
			}
		})
	}
}

// Use 3: the TrainJob drain succeeds once every TrainJob in the namespace
// is gone, and TrainJobs elsewhere do not count.
func TestTrainJobsDrained(t *testing.T) {
	requireEnv(t)
	ns := namespace(t, "tj-drain")
	other := namespace(t, "tj-other")
	createTrainJob(t, ns, "a")
	createTrainJob(t, ns, "b")
	createTrainJob(t, other, "c")
	raw := fixtures.TrainJobsDrained(ns, "60s")
	raw.PollInterval = fixtures.S("1s")
	// Progress goes out on a verdict change or a heartbeat; 2 → 1 remaining
	// is neither, so shorten the heartbeat to see it.
	raw.ProgressInterval = fixtures.S("1s")
	w := startWait(t, raw, reader)
	w.awaitProgress("pending: 2 still match: "+ns+"/a; "+ns+"/b", "\n  "+ns+"/a metadata.name=a")
	deleteTrainJob(t, ns, "a")
	w.awaitProgress("pending: 1 still match: " + ns + "/b")
	deleteTrainJob(t, ns, "b")
	w.expectSuccess()
}

func TestTrainJobsDrainTimesOutNamingLeftovers(t *testing.T) {
	requireEnv(t)
	ns := namespace(t, "tj-stuck")
	createTrainJob(t, ns, "stuck")
	w := startWait(t, fixtures.TrainJobsDrained(ns, "3s"), reader)
	w.expectFailure("kubewait_condition timed out", "1 still match: "+ns+"/stuck", "metadata.name=stuck")
}

func createPod(t *testing.T, ns, name string) {
	t.Helper()
	_, err := kube.CoreV1().Pods(ns).Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

// Use 3: the Pod drain. envtest has no kubelet, so an unscheduled pod is
// deleted at once.
func TestPodsDrained(t *testing.T) {
	requireEnv(t)
	ns := namespace(t, "pods")
	createPod(t, ns, "worker-0")
	createPod(t, ns, "worker-1")
	raw := fixtures.PodsDrained(ns, "60s")
	raw.PollInterval = fixtures.S("1s")
	w := startWait(t, raw, reader)
	w.awaitProgress("pending: 2 still match:", "worker-0 metadata.name=worker-0 spec.nodeName=<none>")
	time.Sleep(500 * time.Millisecond)
	for _, p := range []string{"worker-0", "worker-1"} {
		if err := kube.CoreV1().Pods(ns).Delete(context.Background(), p, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	w.expectSuccess()
}

func createService(t *testing.T, ns, name string, typ corev1.ServiceType) {
	t.Helper()
	// The first Service create can race the ServiceCIDR bootstrap.
	retry(t, func() error {
		_, err := kube.CoreV1().Services(ns).Create(context.Background(), &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: corev1.ServiceSpec{Type: typ, Selector: map[string]string{"app": name},
				Ports: []corev1.ServicePort{{Port: 80}}},
		}, metav1.CreateOptions{})
		return err
	})
}

// Use 5, verbatim: all namespaces, filtered to LoadBalancer. ClusterIP
// Services (including default/kubernetes) are ignored.
func TestNoLoadBalancerServices(t *testing.T) {
	requireEnv(t)
	a, b := namespace(t, "lb-a"), namespace(t, "lb-b")
	createService(t, a, "internal", corev1.ServiceTypeClusterIP)
	createService(t, b, "web", corev1.ServiceTypeLoadBalancer)

	w := startWait(t, fixtures.NoLoadBalancerServices(), reader)
	msg := w.awaitProgress("pending: 1 still match: "+b+"/web", "metadata.namespace="+b+" metadata.name=web status.loadBalancer.ingress=<none>")
	t.Logf("progress: %s", msg)
	w.still(time.Second)
	if err := kube.CoreV1().Services(b).Delete(context.Background(), "web", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	w.expectSuccess()
}
