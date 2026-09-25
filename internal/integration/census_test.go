package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/turfbuild/terraform-provider-kubewait/internal/fixtures"
	"github.com/turfbuild/terraform-provider-kubewait/internal/wait"
)

type nodeSpec struct {
	name          string
	group         string
	gpus          string
	ready         bool
	taints        []corev1.Taint
	unschedulable bool
}

// makeNode creates a Node and sets its status through the status
// subresource, as a kubelet would.
func makeNode(t *testing.T, n nodeSpec) {
	t.Helper()
	ctx := context.Background()
	node, err := kube.CoreV1().Nodes().Create(ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: n.name, Labels: map[string]string{"nodeGroup": n.group}},
		Spec:       corev1.NodeSpec{Taints: n.taints, Unschedulable: n.unschedulable},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	st := corev1.ConditionFalse
	if n.ready {
		st = corev1.ConditionTrue
	}
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: st, Reason: "KubeletReady",
		LastHeartbeatTime: metav1.Now(), LastTransitionTime: metav1.Now()}}
	node.Status.Allocatable = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("96")}
	if n.gpus != "" {
		node.Status.Allocatable["nvidia.com/gpu"] = resource.MustParse(n.gpus)
	}
	if _, err := kube.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func untaint(t *testing.T, name string) {
	t.Helper()
	retry(t, func() error {
		n, err := kube.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		var keep []corev1.Taint
		for _, tn := range n.Spec.Taints {
			if tn.Key != "nodewright.nvidia.com/runtime-required" {
				keep = append(keep, tn)
			}
		}
		n.Spec.Taints = keep
		_, err = kube.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
		return err
	})
}

var nodewright = corev1.Taint{Key: "nodewright.nvidia.com/runtime-required", Value: "true", Effect: corev1.TaintEffectNoSchedule}

// census is use 1 with the fixture's expression and set_expression
// verbatim, on a short clock.
func census(group string, expected int64, names []string, timeout string) wait.Raw {
	raw := fixtures.Census("nodeGroup="+group, expected, names)
	raw.Timeout = fixtures.S(timeout)
	raw.Settle = fixtures.S("2s")
	raw.PollInterval = fixtures.S("1s")
	return raw
}

// Two GPU nodes, one still carrying nodewright's taint; a node in another
// group is outside the selector. Removing the taint mid-wait gives success
// after settle.
func TestCensusTaintRemoved(t *testing.T) {
	requireEnv(t)
	names := []string{"ip-10-0-130-4.ec2.internal", "ip-10-0-131-7.ec2.internal"}
	makeNode(t, nodeSpec{name: names[0], group: "gpu-worker", gpus: "8", ready: true})
	makeNode(t, nodeSpec{name: names[1], group: "gpu-worker", gpus: "8", ready: true, taints: []corev1.Taint{nodewright}})
	makeNode(t, nodeSpec{name: "ip-10-0-1-1.ec2.internal", group: "system", ready: true})

	w := startWait(t, census("gpu-worker", 2, names, "60s"), reader)
	w.awaitProgress("pending: 1/2 pass; need exactly 2; ip-10-0-131-7.ec2.internal: expression false",
		"spec.taints=", "nodewright.nvidia.com/runtime-required", `status.allocatable={"cpu":"96","nvidia.com/gpu":"8"}`)
	w.still(2 * time.Second)
	untaint(t, names[1])
	w.awaitProgress("success: 2/2 pass; need exactly 2")
	w.expectSuccess()
}

// envtest's TaintNodesByCondition admission adds node.kubernetes.io/not-ready
// to every new node; the census expression ignores it, and so must pass.
func TestCensusIgnoresNotReadyTaintKey(t *testing.T) {
	requireEnv(t)
	names := []string{"census-b-1", "census-b-2"}
	for _, n := range names {
		makeNode(t, nodeSpec{name: n, group: "gpu-b", gpus: "8", ready: true})
	}
	n, _ := kube.CoreV1().Nodes().Get(context.Background(), names[0], metav1.GetOptions{})
	t.Logf("taints on a fresh envtest node: %+v", n.Spec.Taints)
	startWait(t, census("gpu-b", 2, names, "30s"), reader).expectSuccess()
}

func TestCensusStaysPending(t *testing.T) {
	requireEnv(t)
	cases := []struct {
		name   string
		nodes  []nodeSpec
		names  []string
		reason string
	}{
		{"extra node", []nodeSpec{{name: "c1-1", gpus: "8", ready: true}, {name: "c1-2", gpus: "8", ready: true}, {name: "c1-3", gpus: "8", ready: true}},
			[]string{"c1-1", "c1-2"}, "3 pass; need exactly 2"},
		{"wrong names", []nodeSpec{{name: "c2-1", gpus: "8", ready: true}, {name: "c2-2", gpus: "8", ready: true}},
			[]string{"c2-1", "c2-9"}, "2/2 pass; set_expression false"},
		{"unschedulable", []nodeSpec{{name: "c3-1", gpus: "8", ready: true}, {name: "c3-2", gpus: "8", ready: true, unschedulable: true}},
			[]string{"c3-1", "c3-2"}, "c3-2: expression false"},
		{"4 GPUs", []nodeSpec{{name: "c4-1", gpus: "8", ready: true}, {name: "c4-2", gpus: "4", ready: true}},
			[]string{"c4-1", "c4-2"}, "c4-2: expression false"},
		{"not ready", []nodeSpec{{name: "c5-1", gpus: "8", ready: true}, {name: "c5-2", gpus: "8"}},
			[]string{"c5-1", "c5-2"}, "c5-2: Ready=False (KubeletReady)"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			group := fmt.Sprintf("gpu-c%d", i)
			for _, n := range tc.nodes {
				n.group = group
				makeNode(t, n)
			}
			w := startWait(t, census(group, 2, tc.names, "4s"), reader)
			w.expectFailure("kubewait_condition timed out", tc.reason, "within timeout (4s)")
		})
	}
}
