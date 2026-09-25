#!/usr/bin/env bash
# Runs TestInvokeInCluster in a pod on a throwaway kind cluster: an empty
# provider configuration must fall back to the pod's service account, as the
# kubernetes provider's does. Needs docker and kind. The cluster's kubeconfig
# stays in .incluster/, so the caller's kubeconfig is never touched.
set -euo pipefail

cluster=${KIND_CLUSTER_NAME:-kubewait-incluster}
image=kubewait-incluster:test
root=$(cd "$(dirname "$0")/.." && pwd)
work=$root/.incluster
kubeconfig=$work/kubeconfig

if kind get clusters 2>/dev/null | grep -qx "$cluster"; then
  echo "kind cluster $cluster already exists; delete it or set KIND_CLUSTER_NAME" >&2
  exit 1
fi

mkdir -p "$work"
arch=$(docker version -f '{{.Server.Arch}}')
(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -c -o "$work/provider.test" ./internal/provider)
printf 'FROM scratch\nCOPY provider.test /provider.test\nENTRYPOINT ["/provider.test"]\n' >"$work/Dockerfile"
docker build -q -t "$image" "$work"

kind create cluster --name "$cluster" --kubeconfig "$kubeconfig" --wait 2m ${KIND_NODE_IMAGE:+--image "$KIND_NODE_IMAGE"}
trap 'kind delete cluster --name "$cluster" --kubeconfig "$kubeconfig"' EXIT
kind load docker-image "$image" --name "$cluster"
k() { kubectl --kubeconfig "$kubeconfig" "$@"; }

# Read-only, like the integration suite's reader.
k apply -f - <<EOF
apiVersion: v1
kind: ServiceAccount
metadata: {name: kubewait, namespace: default}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: kubewait-reader}
rules:
- apiGroups: ["*"]
  resources: ["*"]
  verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: kubewait-reader}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: kubewait-reader}
subjects:
- {kind: ServiceAccount, name: kubewait, namespace: default}
---
apiVersion: v1
kind: Pod
metadata: {name: kubewait-incluster, namespace: default}
spec:
  serviceAccountName: kubewait
  restartPolicy: Never
  containers:
  - name: test
    image: $image
    imagePullPolicy: Never
    args: ["-test.run", "^TestInvokeInCluster\$", "-test.v", "-test.count", "1"]
    env:
    - {name: KUBEWAIT_INCLUSTER, value: "1"}
EOF

phase=
for _ in $(seq 180); do
  phase=$(k get pod kubewait-incluster -o jsonpath='{.status.phase}')
  case $phase in Succeeded | Failed) break ;; esac
  sleep 1
done
k logs kubewait-incluster | tee "$work/test.log" || true

if [ "$phase" = Succeeded ] && grep -q -- '--- PASS: TestInvokeInCluster' "$work/test.log" && ! grep -q -- '--- SKIP' "$work/test.log"; then
  echo "testincluster: PASS"
else
  echo "testincluster: FAIL (pod phase: ${phase:-unknown})" >&2
  exit 1
fi
