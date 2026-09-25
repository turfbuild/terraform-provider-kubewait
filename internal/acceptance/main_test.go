// Package acceptance runs kubewait_condition under a real Terraform (1.16.0
// or later) against envtest. Tests skip unless TF_ACC and KUBEBUILDER_ASSETS
// are set: `make testacc`.
package acceptance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/turfbuild/terraform-provider-kubewait/internal/provider"
)

var (
	env        *envtest.Environment
	startErr   error
	kubeconfig string // the get/list/watch-only user, for the provider
	kube       kubernetes.Interface
	dyn        dynamic.Interface
)

func TestMain(m *testing.M) {
	if os.Getenv("TF_ACC") != "" && os.Getenv("KUBEBUILDER_ASSETS") != "" {
		startErr = start()
	}
	code := m.Run()
	if env != nil {
		_ = env.Stop()
	}
	if kubeconfig != "" {
		_ = os.RemoveAll(filepath.Dir(kubeconfig))
	}
	os.Exit(code)
}

func start() error {
	env = &envtest.Environment{CRDDirectoryPaths: []string{"../integration/testdata/crds"}, ErrorIfCRDPathMissing: true}
	admin, err := env.Start()
	if err != nil {
		return err
	}
	kube = kubernetes.NewForConfigOrDie(admin)
	dyn = dynamic.NewForConfigOrDie(admin)
	ctx := context.Background()
	if _, err := kube.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "kubewait-reader"},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"get", "list", "watch"}}},
	}, metav1.CreateOptions{}); err != nil {
		return err
	}
	if _, err := kube.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "kubewait-reader"},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "kubewait-reader"},
		Subjects:   []rbacv1.Subject{{Kind: "User", Name: "kubewait-reader"}},
	}, metav1.CreateOptions{}); err != nil {
		return err
	}
	u, err := env.AddUser(envtest.User{Name: "kubewait-reader"}, admin)
	if err != nil {
		return err
	}
	cfg := u.Config()
	kc := clientcmdapi.NewConfig()
	kc.Clusters["envtest"] = &clientcmdapi.Cluster{Server: cfg.Host, CertificateAuthorityData: cfg.CAData}
	kc.AuthInfos["reader"] = &clientcmdapi.AuthInfo{ClientCertificateData: cfg.CertData, ClientKeyData: cfg.KeyData}
	kc.Contexts["envtest"] = &clientcmdapi.Context{Cluster: "envtest", AuthInfo: "reader"}
	kc.CurrentContext = "envtest"
	dir, err := os.MkdirTemp("", "kubewait-acc-")
	if err != nil {
		return err
	}
	kubeconfig = filepath.Join(dir, "kubeconfig")
	return clientcmd.WriteToFile(*kc, kubeconfig)
}

func requireEnv(t *testing.T) {
	t.Helper()
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC is not set")
	}
	if env == nil {
		t.Skip("KUBEBUILDER_ASSETS is not set; run `make testacc`")
	}
	if startErr != nil {
		t.Fatalf("envtest failed to start: %v", startErr)
	}
}

// Terraform 1.14 and 1.15 reject destroy triggers and on_failure; 1.16.0 is
// the first release with both. SkipBelow skips silently, so a green run
// counts only with no SKIP lines in -v output.
var terraformChecks = []tfversion.TerraformVersionCheck{
	tfversion.SkipBelow(version.Must(version.NewVersion("1.16.0"))),
}

// The reattached provider is registered under hashicorp/kubewait, so the
// configurations here declare no source for it.
var factories = map[string]func() (tfprotov6.ProviderServer, error){
	"kubewait": providerserver.NewProtocol6WithError(provider.New("acc")()),
}

func hcl(body string) string {
	return fmt.Sprintf("provider \"kubewait\" {\n  config_path = %q\n}\n\n", kubeconfig) + body
}

var nsSeq int

func namespace(t *testing.T) string {
	t.Helper()
	nsSeq++
	name := fmt.Sprintf("acc-%d", nsSeq)
	if _, err := kube.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return name
}

var certGVR = schema.GroupVersionResource{Group: "nvcre.nvidia.com", Version: "v1alpha1", Resource: "certifications"}

func createCertification(t *testing.T, ns string) {
	t.Helper()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "nvcre.nvidia.com/v1alpha1", "kind": "Certification",
		"metadata": map[string]any{"namespace": ns, "name": "gpu-pools"},
		"spec": map[string]any{
			"target":     map[string]any{"nodeNames": []any{"ip-10-0-130-4.ec2.internal"}},
			"categories": []any{map[string]any{"domain": "communication", "variant": "nccl-all-reduce"}},
		},
	}}
	if _, err := dyn.Resource(certGVR).Namespace(ns).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// setCertification sets Succeeded and Failed through the status
// subresource, as the NVCRE controller would.
func setCertification(t *testing.T, ns, succeeded, failed, reason string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	cond := func(typ, status string) map[string]any {
		return map[string]any{"type": typ, "status": status, "reason": reason, "message": reason, "lastTransitionTime": now}
	}
	for i := 0; ; i++ {
		obj, err := dyn.Resource(certGVR).Namespace(ns).Get(context.Background(), "gpu-pools", metav1.GetOptions{})
		if err == nil {
			obj.Object["status"] = map[string]any{"conditions": []any{cond("Succeeded", succeeded), cond("Failed", failed)}}
			_, err = dyn.Resource(certGVR).Namespace(ns).UpdateStatus(context.Background(), obj, metav1.UpdateOptions{})
		}
		if err == nil {
			return
		}
		if i == 20 {
			t.Errorf("set status: %v", err)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func createPod(t *testing.T, ns, name string) {
	t.Helper()
	if _, err := kube.CoreV1().Pods(ns).Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func deletePod(t *testing.T, ns, name string) {
	t.Helper()
	if err := kube.CoreV1().Pods(ns).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		t.Error(err)
	}
}

// later runs f after d, off the test goroutine: the change a wait is
// waiting for, made while Terraform is blocked in the action.
func later(d time.Duration, f func()) {
	go func() {
		time.Sleep(d)
		f()
	}()
}

// expectInvocation checks the plan carries the lifecycle-triggered action
// invocation: the trigger was planned, not just an apply that passed.
type expectInvocation struct{ action, resource, event string }

func (e expectInvocation) CheckPlan(_ context.Context, req plancheck.CheckPlanRequest, resp *plancheck.CheckPlanResponse) {
	norm := func(s string) string { return strings.ToLower(strings.ReplaceAll(s, "_", "")) }
	var have []string
	for _, ai := range req.Plan.ActionInvocations {
		if ai.LifecycleActionTrigger == nil {
			continue
		}
		lt := ai.LifecycleActionTrigger
		have = append(have, fmt.Sprintf("%s by %s on %s", ai.Address, lt.TriggeringResourceAddress, lt.ActionTriggerEvent))
		if ai.Address == e.action && lt.TriggeringResourceAddress == e.resource && norm(lt.ActionTriggerEvent) == norm(e.event) {
			return
		}
	}
	resp.Error = fmt.Errorf("no %s invocation of %s by %s planned; planned: %v", e.event, e.action, e.resource, have)
}

// elapsedAtLeast fails when the apply returned before the change it was
// waiting for could have happened.
func elapsedAtLeast(start *time.Time, d time.Duration) func() error {
	return func() error {
		if took := time.Since(*start); took < d {
			return fmt.Errorf("apply returned after %s; the wait should have held it for at least %s", took, d)
		}
		return nil
	}
}
