# terraform-provider-kubewait

A Terraform provider with one action, `kubewait_condition`. It waits until
Kubernetes objects reach a state:
- a single object's conditions;
- a count, per-object predicate or set predicate over a selected set;
- a drain (nothing left).

Attach it to any lifecycle event. It can gate a create, hold a resource's
dependents until the wait succeeds, gate a teardown, or confirm one.

The action only reads (get, list, watch). It never creates, patches or deletes.

```hcl
provider "kubewait" {
  host                   = module.eks_cluster.endpoint
  cluster_ca_certificate = base64decode(module.eks_cluster.certificate_authority_data)
  exec {
    api_version = "client.authentication.k8s.io/v1beta1"
    command     = "aws"
    args        = ["eks", "get-token", "--cluster-name", module.eks_cluster.cluster_name]
  }
}

resource "kubernetes_manifest" "certification" {
  manifest = { ... }
  lifecycle {
    action_trigger {
      events     = [after_create]
      actions    = [action.kubewait_condition.certification_terminal]
      on_failure = taint
    }
  }
}

action "kubewait_condition" "certification_terminal" {
  config {
    api_version        = "nvcre.nvidia.com/v1alpha1"
    kind               = "Certification"
    namespace          = "nvcre-certification"
    name               = "gpu-pools"
    success_conditions = [{ type = "Succeeded", status = "True" }]
    failure_conditions = [{ type = "Failed", status = "True" }]
    absent             = "failure"
    timeout            = "60m"
    settle             = "2m"
    progress_fields    = ["status.conditions", "status.categoryStatuses"]
  }
}
```

Requires Terraform 1.16.0 or later: destroy-event triggers and `on_failure`
first appear there.

## Provider configuration

The kubernetes provider's connection attributes and `KUBE_*` environment
variables, resolved the same way:
- `host`, `username`, `password`, `insecure`, `tls_server_name`;
- `client_certificate`, `client_key`, `cluster_ca_certificate`;
- `config_path` or `config_paths`, with `config_context`,
  `config_context_auth_info` and `config_context_cluster`;
- `token`, `proxy_url`;
- `exec {}`.

A `provider "kubernetes"` block's connection settings copy over verbatim. With
nothing configured, a wait uses in-cluster credentials when it runs in a pod, as
that provider does, and otherwise fails. It never falls back to localhost.

From a pod, an empty configuration observes the pod's own cluster. A drain of a
kind or namespace that cluster lacks passes at once, so configure destroy-event
drains explicitly.

## Semantics in brief

- **Modes.** Set `name` to observe one object (single-object mode). Otherwise
  the wait observes a set selected by kind, namespace and selectors, narrowed
  by `filter`.
- **Predicates.** `success_conditions` must all hold, ANDed with `expression`.
  Any `failure_conditions`, or `failure_expression`, on any matched object is a
  failure.
- **Set success.** `min_matching ≤ passing ≤ max_matching`, plus
  `require_all` and `set_expression`. `min_matching = 0, max_matching = 0` with
  no success predicate is a drain.
- **Verdict.** Failure wins ties. A verdict must hold for `settle` before it
  counts, and any flip resets the clock. `timeout` is required, and its expiry
  is a failure.
- **Observation.** Watch between resyncs, with a full re-list every
  `poll_interval`.
- **Errors.** API errors are retried until timeout and reset settle. 403 fails
  at once; 401 fails after one immediate retry.
- **Progress** goes out on every verdict change, every `progress_interval`, and
  at the end. It carries the verdict, the reason, the budget, and each object's
  `progress_fields`.

Further behaviour:
- **CEL runtime errors pin the verdict at pending.** They never count as
  success or failure by themselves, so a `filter` error cannot let a drain
  pass. A persistent error ends in a timeout that names it.
- **A kind the server does not serve counts as no objects.** Progress says so,
  and a success reached this way carries a warning naming the kind. Discovery
  runs again on every resync, so a CRD established mid-wait is picked up.
- **Validation runs three times.** `terraform validate` skips values that come
  from variables and data sources; `terraform plan` checks them once known;
  Invoke checks again for engines that call neither. The action never defers
  at plan time and never contacts the cluster before Invoke.
- **`terraform destroy` never lets an action block.** In a destroy walk,
  Terraform (1.16.2, measured) turns a failed destroy-event action into a
  warning whatever `on_failure` says. A `before_destroy` wait halts only a
  destroy inside an ordinary apply, such as a replace.

## Layout

| Path | What |
| --- | --- |
| `internal/wait` | The pure core: parse and validate, CEL, evaluate a snapshot, the settle clock, progress text. No I/O. |
| `internal/watcher` | The loop: discovery, list/watch/resync, the error policy, timers, progress cadence. |
| `internal/provider` | The provider (connection) and the action (schema, ValidateConfig, ModifyPlan, Invoke). |
| `internal/integration` | Waits against a real kube-apiserver and etcd (envtest), including the NVCRE v0.2.0 and Kubeflow Trainer v2.2.0 CRDs in `testdata/crds`. |
| `internal/acceptance` | The action under a real Terraform against envtest. |
| `internal/fixtures` | The five waits of a GPU cluster certification (census, Certification, drains, TrainJob, LoadBalancer Services), for tests at every layer. |

## Building and testing

```sh
make build         # ./terraform-provider-kubewait
make test          # unit tests; the suites below skip without their environment
make testint       # envtest (downloads kube-apiserver and etcd 1.37.0 on first use)
make testacc       # Terraform acceptance: TF_ACC=1, the terraform on PATH (>= 1.16.0), envtest
make testincluster # in a pod on a throwaway kind cluster (docker, kind)
make mirror        # installs 0.1.0 into .mirror/ and writes dev.tfrc
```

The version check skips silently when Terraform is too old. An acceptance run
counts only if its `-v` output has no SKIP lines.

To use a local build from a configuration:

```sh
make mirror
export TF_CLI_CONFIG_FILE=$PWD/dev.tfrc
cd /path/to/configuration && terraform init
```

A rebuilt 0.1.0 has a new checksum, and `init` rejects it even with
`-upgrade`. After each rebuild, run the `terraform providers lock` command
that `make mirror` prints in the configuration, then `init` again.

## License

MPL-2.0.
