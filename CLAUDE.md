# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

A Terraform provider (plugin framework, protocol 6) with one action,
`kubewait_condition`, and no resources or data sources. The action waits
until Kubernetes objects reach a state. It only reads (get, list, watch). The
README is the user-facing spec, and its "Further behaviour" section must stay
in sync with the code.

## Commands

```sh
make build    # ./terraform-provider-kubewait
make test     # go test ./... ; the env-gated suites below skip silently
make lint     # go vet + gofmt check
make fmt
make testint  # envtest: real kube-apiserver + etcd 1.37.0 (downloaded on first use), ~1 min
make testacc  # TF_ACC=1, the terraform on PATH (>= 1.16.0), envtest, ~1 min
make testincluster  # TestInvokeInCluster in a pod on a throwaway kind cluster (docker, kind), ~2 min
make mirror   # local install into .mirror/ plus dev.tfrc; see the README
```

A single test:

```sh
go test ./internal/wait/ -run TestParseValidation -count=1
make testint TESTARGS='-run TestCertificationSucceeded'
make testacc TESTARGS='-run TestAccPlanTimeRejections'
```

`make -s envtest` prints the envtest binaries path, for running `go test` on
the integration or acceptance package directly with `KUBEBUILDER_ASSETS` set.

A suite counts only if its `-v` output has no SKIP lines:
- integration skips without `KUBEBUILDER_ASSETS`;
- acceptance skips without `TF_ACC` and `KUBEBUILDER_ASSETS`, and
  `tfversion.SkipBelow` skips when Terraform is older than 1.16.0;
- `TestInvokeInCluster` skips outside a pod.

## Architecture

Three layers, each with its own tests:

1. **`internal/provider`** turns Terraform config into a `wait.Raw` and builds
   clients.
   - `parseConfig` (`condition_model.go`) is the single parser. It is called
     from `ValidateConfig` (errors and warnings), `ModifyPlan` (errors only)
     and `Invoke` (errors again, for engines that skip validate and plan).
   - The model uses framework value types throughout because any value may be
     unknown at validate or plan time.
   - `Configure` never contacts the cluster and never defers. An
     unknown provider config leaves `Connection.Settings` nil.
2. **`internal/wait`** is the pure core, with no I/O.
   - `Parse(Raw) → (*Spec, Problems)`: `wait.Str/Num/Bool` carry `Set` and
     `Unknown`, and checks involving an unknown are skipped.
   - `Evaluate(spec, objects) → Outcome`.
   - `Tracker` is the settle clock; `FormatProgress` renders progress text.
   - Defaults live in constants here, because action schemas allow no
     Computed or Default attributes.
3. **`internal/watcher`** runs the loop.
   - `Runner.Run` resolves the kind through discovery on every cycle, re-lists,
     and watches until the next resync. It applies the error policy and
     progress cadence, and feeds each snapshot to `wait`.
   - `Source` (List and Watch only) and `Resolver` are interfaces. The unit
     tests use a scripted `fakeSource` and a fake clock. `Runner.idle` lets them
     advance the clock in lockstep.

`internal/fixtures` holds five real-world waits (a GPU node census, a
Certification, TrainJob and Pod drains, TrainJob completion, a LoadBalancer
Service drain). They are reused by the tests at every layer; the tests' comments
refer to them as numbered "uses". The integration and acceptance suites install
the vendored CRDs in `internal/integration/testdata/crds`. They run every wait
as a user bound to get/list/watch only, so any mutation would fail.

## Constraints

- **Schema parity.** The action schema must stay attribute-for-attribute
  identical to `internal/provider/testdata/dynamic_resources.json`, a
  tfcoremock stand-in that existing configurations were type-checked against.
  `TestActionSchemaMatchesStandIn` enforces this. A schema change therefore
  also changes the stand-in and the configurations checked against it. Raise
  it before making one.
- **Read-only.** Never add a create, patch or delete call.
- **Deliberate semantics.** Ask before changing any of these:
  - CEL runtime errors pin the verdict at pending;
  - API errors reset settle;
  - a kind the server does not serve counts as no objects, with a warning;
  - there is no plan-time deferral, and no contact with the cluster before
    `Invoke`.
- **Connection parity with hashicorp/kubernetes.** `connection.go` resolves
  settings the way that provider does: `clientcmd`, `KUBE_*` defaults, and
  the in-cluster fallback. The one intended divergence is `ErrNoCluster`
  instead of a localhost fallback. That provider does not read
  `~/.kube/config` by default, and neither does this one; that is not a gap.
  Unit tests cannot exercise a successful in-cluster connection;
  `make testincluster` covers it.
- **Progress.** The framework's `SendProgress` is an unbuffered channel send.
  Always go through `sendProgress`, which gives up on `ctx.Done()`.
- **Acceptance configs.**
  - A reattached provider registers as `hashicorp/kubewait`, so test configs
    must not declare `source`.
  - Destroy-event action configs must be literals.
  - In `terraform destroy`, Terraform turns a failed action into a warning
    whatever `on_failure` says. The tests pin this as measured on 1.16.2.
- The CEL module path is `cel.dev/cel-go`, not `github.com/google/cel-go`.
- This repo is public and standalone. Code, comments and docs describe
  behaviour. They name no downstream consumer, internal project or ticket id.
