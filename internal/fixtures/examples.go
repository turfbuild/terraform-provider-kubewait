// Package fixtures holds the five kubewait_condition configurations of a GPU
// cluster certification, for tests at every layer. The "uses" numbered in
// comments across the tests:
//
//  1. gpu_census: exactly the expected GPU nodes, Ready, untainted, advertising GPUs;
//  2. certification_terminal: an NVCRE Certification reaches Succeeded or Failed;
//  3. trainjobs_drained, pods_drained: nothing left in the namespace;
//  4. trainjob_finished: a Kubeflow TrainJob reaches Complete or Failed;
//  5. no_load_balancer_services: no LoadBalancer Service in any namespace.
package fixtures

import (
	"encoding/json"
	"math/big"

	"github.com/turfbuild/terraform-provider-kubewait/internal/wait"
)

func S(v string) wait.Str { return wait.Str{V: v, Set: true} }
func N(n int64) wait.Num  { return wait.Num{V: new(big.Float).SetInt64(n), Set: true} }
func B(b bool) wait.Bool  { return wait.Bool{V: b, Set: true} }
func Ss(vs ...string) wait.Strs {
	out := wait.Strs{Set: true}
	for _, v := range vs {
		out.Items = append(out.Items, S(v))
	}
	return out
}
func C(typ, status string) wait.RawCondition {
	return wait.RawCondition{Type: S(typ), Status: S(status)}
}
func Cs(cs ...wait.RawCondition) wait.Conds { return wait.Conds{Items: cs, Set: true} }

// CensusExpression is the gpu_census expression as an HCL heredoc renders it, with gpus_per_node = 8.
const CensusExpression = `!(has(object.spec.unschedulable) && object.spec.unschedulable) &&
!(has(object.spec.taints) && object.spec.taints.exists(t, t.effect == "NoSchedule" &&
    (t.key.startsWith("nodewright.nvidia.com") || t.key.startsWith("skyhook.nvidia.com")))) &&
has(object.status.allocatable) && "nvidia.com/gpu" in object.status.allocatable &&
int(object.status.allocatable["nvidia.com/gpu"]) >= 8
`

// CensusSetExpression renders gpu_census's set_expression for node names,
// as jsonencode(local.node_names) would.
func CensusSetExpression(names []string) string {
	js, _ := json.Marshal(names)
	return "objects.all(o, o.metadata.name in " + string(js) + ")"
}

// Census is use 1, gpu_census.
func Census(labelSelector string, expected int64, names []string) wait.Raw {
	return wait.Raw{
		APIVersion:        S("v1"),
		Kind:              S("Node"),
		LabelSelector:     S(labelSelector),
		MinMatching:       N(expected),
		MaxMatching:       N(expected),
		RequireAll:        B(true),
		SuccessConditions: Cs(C("Ready", "True")),
		Expression:        S(CensusExpression),
		SetExpression:     S(CensusSetExpression(names)),
		Timeout:           S("30m"),
		Settle:            S("20s"),
		PollInterval:      S("10s"),
		ProgressFields:    Ss("metadata.name", "spec.taints", "status.allocatable"),
	}
}

// CertificationTerminal is use 2, certification_terminal.
func CertificationTerminal(namespace, name, timeout, settle string) wait.Raw {
	return wait.Raw{
		APIVersion:        S("nvcre.nvidia.com/v1alpha1"),
		Kind:              S("Certification"),
		Namespace:         S(namespace),
		Name:              S(name),
		SuccessConditions: Cs(C("Succeeded", "True")),
		FailureConditions: Cs(C("Failed", "True")),
		Absent:            S("failure"),
		Timeout:           S(timeout),
		Settle:            S(settle),
		PollInterval:      S("30s"),
		ProgressFields:    Ss("status.conditions", "status.categoryStatuses"),
		ProgressInterval:  S("2m"),
	}
}

// TrainJobsDrained is use 3, trainjobs_drained.
func TrainJobsDrained(namespace, timeout string) wait.Raw {
	return wait.Raw{
		APIVersion:     S("trainer.kubeflow.org/v1alpha1"),
		Kind:           S("TrainJob"),
		Namespace:      S(namespace),
		MinMatching:    N(0),
		MaxMatching:    N(0),
		Timeout:        S(timeout),
		ProgressFields: Ss("metadata.name"),
	}
}

// PodsDrained is use 3, pods_drained.
func PodsDrained(namespace, timeout string) wait.Raw {
	return wait.Raw{
		APIVersion:     S("v1"),
		Kind:           S("Pod"),
		Namespace:      S(namespace),
		MinMatching:    N(0),
		MaxMatching:    N(0),
		Timeout:        S(timeout),
		ProgressFields: Ss("metadata.name", "spec.nodeName"),
	}
}

// TrainJobFinished is use 4, trainjob_finished.
func TrainJobFinished(namespace, name, timeout string) wait.Raw {
	return wait.Raw{
		APIVersion:        S("trainer.kubeflow.org/v1alpha1"),
		Kind:              S("TrainJob"),
		Namespace:         S(namespace),
		Name:              S(name),
		SuccessConditions: Cs(C("Complete", "True")),
		FailureConditions: Cs(C("Failed", "True")),
		Absent:            S("failure"),
		Timeout:           S(timeout),
		PollInterval:      S("15s"),
		ProgressFields:    Ss("status.conditions"),
	}
}

// NoLoadBalancerServices is use 5, no_load_balancer_services, verbatim
// (it is all literals).
func NoLoadBalancerServices() wait.Raw {
	return wait.Raw{
		APIVersion:     S("v1"),
		Kind:           S("Service"),
		Filter:         S("object.spec.type == 'LoadBalancer'"),
		MinMatching:    N(0),
		MaxMatching:    N(0),
		Timeout:        S("10m"),
		PollInterval:   S("10s"),
		ProgressFields: Ss("metadata.namespace", "metadata.name", "status.loadBalancer.ingress"),
	}
}
