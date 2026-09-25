package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/turfbuild/terraform-provider-kubewait/internal/fixtures"
	"github.com/turfbuild/terraform-provider-kubewait/internal/wait"
)

// A user with no role gets 403 on the list: the wait fails at once rather
// than waiting out its timeout.
func TestForbiddenFailsFast(t *testing.T) {
	requireEnv(t)
	w := startWait(t, fixtures.PodsDrained(namespace(t, "forbidden"), "60s"), unbound)
	w.expectFailure("kubewait_condition cannot read v1 Pod", "403 Forbidden", "get, list and watch")
	if _, took := w.finish(); took > 5*time.Second {
		t.Errorf("took %s", took)
	}
}

// Credentials the server rejects outright (401) fail after the one
// immediate retry, well before the timeout.
func TestUnauthorizedFailsFast(t *testing.T) {
	requireEnv(t)
	bad := rest.CopyConfig(reader)
	bad.CertData, bad.KeyData = nil, nil
	bad.BearerToken = "not-a-token"
	w := startWait(t, fixtures.PodsDrained(namespace(t, "unauthorized"), "60s"), bad)
	r, took := w.finish()
	if r.Verdict != wait.Failure || r.Summary != "kubewait_condition is not authenticated" || !strings.Contains(r.Detail, "401") {
		t.Fatalf("got %s %q\n%s", r.Verdict, r.Summary, r.Detail)
	}
	if took > 5*time.Second {
		t.Errorf("took %s", took)
	}
}

var widgetGVR = schema.GroupVersionResource{Group: "example.kubewait.dev", Version: "v1", Resource: "widgets"}

func widgetRaw(ns string) wait.Raw {
	return wait.Raw{APIVersion: fixtures.S("example.kubewait.dev/v1"), Kind: fixtures.S("Widget"), Namespace: fixtures.S(ns),
		Timeout: fixtures.S("60s"), PollInterval: fixtures.S("1s")}
}

// A drain on a kind nobody serves passes, with a warning naming the kind;
// a success wait on it stays pending until its CRD is installed mid-wait
// and an object appears.
func TestUnservedKind(t *testing.T) {
	requireEnv(t)
	ns := namespace(t, "unserved")
	drain := widgetRaw(ns)
	drain.MinMatching, drain.MaxMatching = fixtures.N(0), fixtures.N(0)
	r := startWait(t, drain, reader).expectSuccess()
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "does not serve example.kubewait.dev/v1 Widget") {
		t.Errorf("warnings: %v", r.Warnings)
	}

	w := startWait(t, widgetRaw(ns), reader)
	w.awaitProgress("pending: 0/0 pass; need at least 1 [kind example.kubewait.dev/v1 Widget not served (treated as no objects)]")
	w.still(2 * time.Second)
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.kubewait.dev"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.kubewait.dev",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "widgets", Singular: "widget", Kind: "Widget", ListKind: "WidgetList"},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", XPreserveUnknownFields: ptr(true)}}}},
		},
	}
	if _, err := envtest.InstallCRDs(admin, envtest.CRDInstallOptions{CRDs: []*apiextensionsv1.CustomResourceDefinition{crd}}); err != nil {
		t.Fatal(err)
	}
	w.still(time.Second)
	if _, err := dyn.Resource(widgetGVR).Namespace(ns).Create(context.Background(), &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.kubewait.dev/v1", "kind": "Widget", "metadata": map[string]any{"name": "w"}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if r := w.expectSuccess(); len(r.Warnings) != 0 {
		t.Errorf("warnings after the CRD was served: %v", r.Warnings)
	}
}

func ptr[T any](v T) *T { return &v }
