package wait_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/json"

	"github.com/turfbuild/terraform-provider-kubewait/internal/fixtures"
	"github.com/turfbuild/terraform-provider-kubewait/internal/wait"
)

// All five example configurations parse with no errors and no warnings.
func TestExampleConfigsAccepted(t *testing.T) {
	names := []string{"ip-10-0-130-4.ec2.internal", "ip-10-0-131-7.ec2.internal"}
	for name, raw := range map[string]wait.Raw{
		"gpu_census":                fixtures.Census("nodeGroup=gpu-worker", 2, names),
		"certification_terminal":    fixtures.CertificationTerminal("nvcre-certification", "gpu-pools", "60m", "2m"),
		"trainjobs_drained":         fixtures.TrainJobsDrained("nvcre-certification", "10m"),
		"pods_drained":              fixtures.PodsDrained("nvcre-certification", "10m"),
		"trainjob_finished":         fixtures.TrainJobFinished("kubeflow", "pytorch-mnist", "20m"),
		"no_load_balancer_services": fixtures.NoLoadBalancerServices(),
	} {
		s, probs := wait.Parse(raw)
		if len(probs) != 0 || s == nil {
			t.Errorf("%s: %+v", name, probs)
		}
	}
}

type nodeOpts struct {
	name          string
	ready         string
	gpus          string // "" omits nvidia.com/gpu
	noAllocatable bool
	unschedulable bool
	taints        []string // key:effect
}

func node(t *testing.T, o nodeOpts) map[string]any {
	t.Helper()
	if o.ready == "" {
		o.ready = "True"
	}
	var taints []string
	for _, tk := range o.taints {
		k, e, _ := strings.Cut(tk, ":")
		taints = append(taints, fmt.Sprintf(`{"key":%q,"effect":%q}`, k, e))
	}
	spec := fmt.Sprintf(`{"taints":[%s]}`, strings.Join(taints, ","))
	if o.unschedulable {
		spec = fmt.Sprintf(`{"unschedulable":true,"taints":[%s]}`, strings.Join(taints, ","))
	}
	alloc := `,"allocatable":{"cpu":"96","memory":"1Ti"`
	if o.gpus != "" {
		alloc += fmt.Sprintf(`,"nvidia.com/gpu":%q`, o.gpus)
	}
	alloc += "}"
	if o.noAllocatable {
		alloc = ""
	}
	js := fmt.Sprintf(`{"apiVersion":"v1","kind":"Node","metadata":{"name":%q,"labels":{"nodeGroup":"gpu-worker"}},
		"spec":%s,"status":{"conditions":[{"type":"Ready","status":%q,"reason":"KubeletReady"}]%s}}`, o.name, spec, o.ready, alloc)
	var m map[string]any
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The census expression verbatim, against Node fixtures.
func TestCensus(t *testing.T) {
	names := []string{"gpu-a", "gpu-b"}
	s, probs := wait.Parse(fixtures.Census("nodeGroup=gpu-worker", 2, names))
	if s == nil {
		t.Fatal(probs)
	}
	good := func(name string) nodeOpts { return nodeOpts{name: name, gpus: "8"} }
	cases := []struct {
		name   string
		b      nodeOpts
		extra  []nodeOpts
		want   wait.Verdict
		reason string
	}{
		{name: "both ready", b: good("gpu-b"), want: wait.Success, reason: "2/2 pass; need exactly 2"},
		{name: "nodewright taint", b: nodeOpts{name: "gpu-b", gpus: "8", taints: []string{"nodewright.nvidia.com/runtime-required:NoSchedule"}},
			want: wait.Pending, reason: "gpu-b: expression false"},
		{name: "legacy skyhook taint", b: nodeOpts{name: "gpu-b", gpus: "8", taints: []string{"skyhook.nvidia.com/x:NoSchedule"}},
			want: wait.Pending, reason: "gpu-b: expression false"},
		{name: "not-ready taint is ignored", b: nodeOpts{name: "gpu-b", gpus: "8", taints: []string{"node.kubernetes.io/not-ready:NoSchedule"}},
			want: wait.Success},
		{name: "nodewright PreferNoSchedule is ignored", b: nodeOpts{name: "gpu-b", gpus: "8", taints: []string{"nodewright.nvidia.com/x:PreferNoSchedule"}},
			want: wait.Success},
		{name: "unschedulable", b: nodeOpts{name: "gpu-b", gpus: "8", unschedulable: true}, want: wait.Pending, reason: "expression false"},
		{name: "no allocatable", b: nodeOpts{name: "gpu-b", noAllocatable: true}, want: wait.Pending, reason: "expression false"},
		{name: "no gpu resource", b: nodeOpts{name: "gpu-b"}, want: wait.Pending, reason: "expression false"},
		{name: "4 GPUs instead of 8", b: nodeOpts{name: "gpu-b", gpus: "4"}, want: wait.Pending, reason: "1/2 pass; need exactly 2; gpu-b: expression false"},
		{name: "not ready", b: nodeOpts{name: "gpu-b", gpus: "8", ready: "False"}, want: wait.Pending, reason: "gpu-b: Ready=False (KubeletReady)"},
		{name: "extra node", b: good("gpu-b"), extra: []nodeOpts{good("gpu-c")}, want: wait.Pending, reason: "3 pass; need exactly 2"},
		{name: "wrong name", b: good("gpu-z"), want: wait.Pending, reason: "set_expression false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []map[string]any{node(t, good("gpu-a")), node(t, tc.b)}
			for _, e := range tc.extra {
				objs = append(objs, node(t, e))
			}
			out := wait.Evaluate(s, objs)
			if out.Verdict != tc.want || !strings.Contains(out.Reason, tc.reason) || len(out.EvalErrors) != 0 {
				t.Errorf("got %s %q (errors %v), want %s containing %q", out.Verdict, out.Reason, out.EvalErrors, tc.want, tc.reason)
			}
		})
	}
}

func TestFormatProgress(t *testing.T) {
	s, _ := wait.Parse(fixtures.CertificationTerminal("nvcre-certification", "gpu-pools", "60m", "2m"))
	var o map[string]any
	_ = json.Unmarshal([]byte(`{"metadata":{"namespace":"nvcre-certification","name":"gpu-pools"},
		"status":{"conditions":[{"type":"Succeeded","status":"False","reason":"WorkflowRunning"},{"type":"Failed","status":"False","reason":"WorkflowRunning"}],
		"categoryStatuses":[{"domain":"communication","variant":"nccl-all-reduce","status":"InProgress"}]}}`), &o)
	out := wait.Evaluate(s, []map[string]any{o})
	msg := wait.FormatProgress(s, wait.Snapshot{Outcome: out, Elapsed: 12 * time.Minute, Remaining: 48 * time.Minute, Settle: s.Settle})
	want := "pending: nvcre-certification/gpu-pools: Succeeded=False (WorkflowRunning) · 12m elapsed, 48m left\n" +
		"  nvcre-certification/gpu-pools status.conditions=[Succeeded=False(WorkflowRunning) Failed=False(WorkflowRunning)]" +
		` status.categoryStatuses=[{"domain":"communication","status":"InProgress","variant":"nccl-all-reduce"}]`
	if msg != want {
		t.Errorf("got:\n%s\nwant:\n%s", msg, want)
	}

	// Settling and a note.
	_ = json.Unmarshal([]byte(`{"metadata":{"namespace":"nvcre-certification","name":"gpu-pools"},
		"status":{"conditions":[{"type":"Succeeded","status":"True","reason":"AllPassed"}]}}`), &o)
	out = wait.Evaluate(s, []map[string]any{o})
	msg = wait.FormatProgress(s, wait.Snapshot{Outcome: out, Elapsed: 20 * time.Minute, Remaining: 40 * time.Minute,
		Settling: 40 * time.Second, Settle: s.Settle, Note: "retrying: 503"})
	if !strings.HasPrefix(msg, "success: nvcre-certification/gpu-pools: Succeeded=True [retrying: 503] · 20m elapsed, 40m left · success held 40s of 2m\n") ||
		!strings.Contains(msg, "status.categoryStatuses=<none>") {
		t.Errorf("got:\n%s", msg)
	}

	for d, want := range map[time.Duration]string{0: "0s", 40 * time.Second: "40s", 90 * time.Minute: "1h30m", time.Hour + 5*time.Second: "1h5s", -time.Second: "0s"} {
		if got := wait.FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%s) = %s, want %s", d, got, want)
		}
	}
}
