# terraform-provider-kubewait

A Terraform provider with one action, `kubewait_condition`. It waits until
Kubernetes objects reach a state:
- one object's conditions or fields;
- a count or predicate over a set of objects;
- a drain, where nothing is left.

Attach it to a resource's lifecycle events to hold the resource's dependents
until the wait succeeds, to gate a teardown, or to confirm one. The action only
reads (get, list, watch). It never creates, patches or deletes.

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

resource "kubernetes_manifest" "migrate" {
  manifest = yamldecode(file("${path.module}/migrate-job.yaml"))
  lifecycle {
    action_trigger {
      events     = [after_create]
      actions    = [action.kubewait_condition.migrate_done]
      on_failure = taint
    }
  }
}

action "kubewait_condition" "migrate_done" {
  config {
    api_version        = "batch/v1"
    kind               = "Job"
    namespace          = "app"
    name               = "migrate"
    success_conditions = [{ type = "Complete", status = "True" }]
    failure_conditions = [{ type = "Failed", status = "True" }]
    absent             = "failure"
    timeout            = "15m"
    progress_fields    = ["status.active", "status.succeeded", "status.failed"]
  }
}
```

Anything that depends on `kubernetes_manifest.migrate` waits for the Job to
finish. Retries count as pending, because the Job controller sets `Failed` only
once `backoffLimit` is spent. A failure halts the apply. `on_failure = taint`
also marks the Job for replacement, so the next apply runs it again.

Requires Terraform 1.16.0 or later: destroy-event triggers and `on_failure`
first appear there.

## Examples

### Attaching a wait

| Event | The wait holds |
| --- | --- |
| `after_create`, `after_update` | the resource's dependents |
| `before_destroy` | the resource's destroy |
| `after_destroy` | whatever the resource depends on |

A failed wait halts the apply. `on_failure = continue` turns the failure into a
warning, and `on_failure = taint` also marks the resource for replacement.

To run a wait by itself, use
`terraform apply -invoke=action.kubewait_condition.<name>`.

### A Deployment finishes rolling out

The rollout is done when every replica of the latest spec is updated and
available. Check the counts rather than `Available=True`, which also holds
mid-rollout. A rollout that stops progressing for `progressDeadlineSeconds`
(default 600) fails the wait. Trigger it on `after_create` and `after_update`
of whatever changes the Deployment.

```hcl
action "kubewait_condition" "api_rolled_out" {
  config {
    api_version = "apps/v1"
    kind        = "Deployment"
    namespace   = "app"
    name        = "api"
    expression  = <<-EOT
      has(object.status.observedGeneration) &&
      object.status.observedGeneration >= object.metadata.generation &&
      has(object.status.updatedReplicas) && object.status.updatedReplicas == object.spec.replicas &&
      has(object.status.availableReplicas) && object.status.availableReplicas == object.spec.replicas &&
      object.status.replicas == object.spec.replicas
    EOT
    failure_conditions = [
      { type = "Progressing", status = "False", reason = "ProgressDeadlineExceeded" },
    ]
    timeout         = "15m"
    progress_fields = ["status.updatedReplicas", "status.availableReplicas", "status.conditions"]
  }
}
```

### Nodes join and advertise their GPUs

The wait needs at least four nodes from the pool, and every one of them Ready
and advertising eight GPUs. `settle` rides out a node that flaps while it
boots. Node-pool labels differ by platform, such as
`cloud.google.com/gke-nodepool` on GKE or `karpenter.sh/nodepool` with
Karpenter. Without GPUs, drop `expression`.

```hcl
action "kubewait_condition" "gpu_nodes_ready" {
  config {
    api_version        = "v1"
    kind               = "Node"
    label_selector     = "eks.amazonaws.com/nodegroup=gpu"
    min_matching       = 4
    require_all        = true
    success_conditions = [{ type = "Ready", status = "True" }]
    expression         = "'nvidia.com/gpu' in object.status.allocatable && int(object.status.allocatable['nvidia.com/gpu']) >= 8"
    settle             = "30s"
    timeout            = "30m"
    progress_fields    = ["metadata.name", "status.allocatable[\"nvidia.com/gpu\"]"]
  }
}
```

### A Service gets its load balancer

This holds dependents until the cloud has provisioned the Service's load
balancer.

```hcl
action "kubewait_condition" "gateway_address" {
  config {
    api_version     = "v1"
    kind            = "Service"
    namespace       = "gateway"
    name            = "gateway"
    expression      = "has(object.status.loadBalancer.ingress)"
    timeout         = "10m"
    progress_fields = ["status.loadBalancer"]
  }
}
```

### An operator's resource is Ready

Many operators report a `Ready` condition, and cert-manager's Certificate is
one. Until the CRD is installed, the kind counts as no objects, so the wait
stays pending rather than failing.

```hcl
action "kubewait_condition" "api_tls" {
  config {
    api_version        = "cert-manager.io/v1"
    kind               = "Certificate"
    namespace          = "app"
    name               = "api-tls"
    success_conditions = [{ type = "Ready", status = "True" }]
    timeout            = "10m"
    progress_fields    = ["status.conditions"]
  }
}
```

### An Argo CD Application is synced and healthy

Argo CD reports sync and health as status fields, not conditions, so this wait
uses CEL. A field that isn't there yet is a CEL error, which keeps the wait
pending, so guard each level with `has()`. `settle` gives a brief `Degraded`
two minutes to recover before it fails the wait.

```hcl
action "kubewait_condition" "platform_synced" {
  config {
    api_version        = "argoproj.io/v1alpha1"
    kind               = "Application"
    namespace          = "argocd"
    name               = "platform"
    expression         = <<-EOT
      has(object.status) && has(object.status.sync) && has(object.status.health) &&
      object.status.sync.status == 'Synced' && object.status.health.status == 'Healthy'
    EOT
    failure_expression = "has(object.status) && has(object.status.health) && object.status.health.status == 'Degraded'"
    settle             = "2m"
    timeout            = "30m"
    progress_fields    = ["status.sync.status", "status.health.status"]
  }
}
```

### No load balancers left before the cluster goes

A Service of type LoadBalancer creates a cloud load balancer outside
Terraform's state. If the cluster or its VPC is destroyed first, the load
balancer leaks, and it can block the VPC's deletion. The Service keeps a
finalizer until its load balancer is deleted. So once no LoadBalancer Service
is left, the load balancers are gone too.

A `terraform_data` barrier puts the wait in between. In-cluster resources
depend on it, and it depends on the cluster. So it is destroyed after them and
before the cluster.

```hcl
resource "terraform_data" "cluster_contents" {
  triggers_replace = module.eks_cluster.endpoint
  depends_on       = [module.eks_nodes]
  lifecycle {
    action_trigger {
      events  = [before_destroy]
      actions = [action.kubewait_condition.no_load_balancers]
    }
  }
}

resource "helm_release" "gateway" {
  # ...
  depends_on = [terraform_data.cluster_contents]
}

action "kubewait_condition" "no_load_balancers" {
  config {
    api_version     = "v1"
    kind            = "Service"
    filter          = "object.spec.type == 'LoadBalancer'"
    min_matching    = 0
    max_matching    = 0
    timeout         = "10m"
    progress_fields = ["metadata.namespace", "metadata.name"]
  }
}
```

The wait only observes. A Service created outside Terraform holds the teardown
until someone deletes it. Terraform must know a destroy-event action's
`config` when it plans the destroy, so write the config with literals. Under
`terraform destroy`, a timed-out wait becomes a warning, but the teardown
still waits for it to end (see [How a wait works](#how-a-wait-works)).

### A release's Pods are gone after uninstall

Pods can outlive the release that created them while they terminate. This
holds whatever the release depends on, such as its node group, until they are
gone.

```hcl
resource "helm_release" "training" {
  # ...
  lifecycle {
    action_trigger {
      events  = [after_destroy]
      actions = [action.kubewait_condition.training_pods_gone]
    }
  }
}

action "kubewait_condition" "training_pods_gone" {
  config {
    api_version     = "v1"
    kind            = "Pod"
    namespace       = "training"
    min_matching    = 0
    max_matching    = 0
    timeout         = "5m"
    progress_fields = ["metadata.name", "spec.nodeName"]
  }
}
```

## Provider configuration

The provider takes the kubernetes provider's connection attributes and `KUBE_*`
environment variables, and resolves them the same way:
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

## How a wait works

- **Mode.**
  - With `name` set, the wait observes one object. `absent` says what a missing
    object counts as (default pending).
  - Otherwise it observes a set. The set is selected by kind, `namespace`,
    `label_selector` and `field_selector`, then narrowed by `filter`.
- **Verdict.**
  - An object passes when all `success_conditions` hold and `expression` is
    true. It fails on any `failure_conditions` entry or `failure_expression`.
    Conditions match `status.conditions` exactly; `status.phase` is not
    consulted.
  - A set succeeds when `min_matching` (default 1) to `max_matching` objects
    pass, every matched object passes if `require_all` is set, and
    `set_expression` holds over `objects`.
  - `min_matching = 0, max_matching = 0` with no success predicate is a drain.
  - Failure wins ties.
- **Time.**
  - A verdict counts once it has held for `settle`; any flip resets the clock.
  - `timeout` is required, and its expiry is a failure.
  - The wait watches between full re-lists every `poll_interval` (default 10s).
- **Errors.**
  - API errors are retried until timeout and reset settle. 403 fails at once;
    401 fails after one immediate retry.
  - A CEL runtime error keeps the verdict pending. It never counts as success
    or failure, so a `filter` error cannot let a drain pass. A persistent error
    ends in a timeout that names it.
  - A kind the server does not serve counts as no objects. Progress says so,
    and a success reached this way carries a warning. Discovery runs again on
    every re-list, so a CRD established mid-wait is picked up.
- **Progress** goes out on every verdict change, every `progress_interval`
  (default 60s), and at the end. It carries the verdict, the reason, the
  budget, and each object's `progress_fields`.
- **Validation.** `terraform validate` checks what it can, and skips values
  that come from variables and data sources. `terraform plan` checks those once
  they are known, and the action checks again when invoked. Nothing contacts the
  cluster before invoke, and the action never defers.
- **`terraform destroy` never fails on a wait.** In a destroy walk, Terraform
  (1.16.2, measured) still waits for a destroy-event action to end, then turns
  a failure into a warning whatever `on_failure` says. A `before_destroy` wait
  halts only a destroy inside an ordinary apply, such as a replace.

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
