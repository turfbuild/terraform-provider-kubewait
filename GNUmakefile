VERSION             ?= 0.1.0
ENVTEST_K8S_VERSION ?= 1.37.0
SETUP_ENVTEST       ?= go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.25
OS_ARCH             := $(shell go env GOOS)_$(shell go env GOARCH)
MIRROR              := $(CURDIR)/.mirror
PLUGIN_DIR          := $(MIRROR)/registry.terraform.io/turfbuild/kubewait/$(VERSION)/$(OS_ARCH)

# Evaluated only by the targets that use it: downloads kube-apiserver and
# etcd on first use.
KUBEBUILDER_ASSETS = $(shell $(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)

default: build

.PHONY: build
build:
	go build -ldflags "-X main.version=$(VERSION)" -o terraform-provider-kubewait .

# Unit tests. The integration and acceptance suites skip without their
# environment.
.PHONY: test
test:
	go test ./... -count=1

.PHONY: envtest
envtest:
	@echo "$(KUBEBUILDER_ASSETS)"

# Waits against a real kube-apiserver and etcd (envtest).
.PHONY: testint
testint:
	KUBEBUILDER_ASSETS="$(KUBEBUILDER_ASSETS)" go test ./internal/integration/ -v -count=1 -timeout 20m $(TESTARGS)

# The action under a real Terraform, against envtest. TF_ACC_TERRAFORM_PATH
# pins the terraform on PATH, so plugin-testing cannot download another.
.PHONY: testacc
testacc:
	TF_ACC=1 TF_ACC_TERRAFORM_PATH="$$(command -v terraform)" KUBEBUILDER_ASSETS="$(KUBEBUILDER_ASSETS)" \
		go test ./internal/acceptance/ -v -count=1 -timeout 30m $(TESTARGS)

# TestInvokeInCluster in a pod on a throwaway kind cluster: an empty provider
# configuration falls back to the pod's service account. Needs docker and kind.
.PHONY: testincluster
testincluster:
	./hack/testincluster.sh

# Installs the provider into an unpacked filesystem mirror and writes a CLI
# configuration that serves turfbuild/kubewait from it. Use it with
#   TF_CLI_CONFIG_FILE=$(CURDIR)/dev.tfrc terraform init
# Rebuilding the same version changes its checksum, and init rejects it even
# with -upgrade. The command printed last records the new checksum in a
# configuration's lock file.
.PHONY: mirror
mirror:
	mkdir -p "$(PLUGIN_DIR)"
	go build -ldflags "-X main.version=$(VERSION)" -o "$(PLUGIN_DIR)/terraform-provider-kubewait_v$(VERSION)" .
	printf '%s\n' \
		'provider_installation {' \
		'  filesystem_mirror {' \
		'    path    = "$(MIRROR)"' \
		'    include = ["registry.terraform.io/turfbuild/kubewait"]' \
		'  }' \
		'  direct {' \
		'    exclude = ["registry.terraform.io/turfbuild/kubewait"]' \
		'  }' \
		'}' > dev.tfrc
	@echo "wrote dev.tfrc; export TF_CLI_CONFIG_FILE=$(CURDIR)/dev.tfrc"
	@echo "after a rebuild, in each configuration that uses it:"
	@echo "  terraform providers lock -fs-mirror=$(MIRROR) -platform=$(OS_ARCH) registry.terraform.io/turfbuild/kubewait"

.PHONY: fmt
fmt:
	gofmt -w .

.PHONY: lint
lint:
	go vet ./...
	test -z "$$(gofmt -l .)"
