package watcher

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/clock"

	"github.com/turfbuild/terraform-provider-kubewait/internal/wait"
)

func TestDiscoveryResolver(t *testing.T) {
	fd := &fakediscovery.FakeDiscovery{Fake: &k8stesting.Fake{}}
	fd.Resources = []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "pods", Kind: "Pod", Namespaced: true},
			{Name: "pods/status", Kind: "Pod", Namespaced: true},
			{Name: "nodes", Kind: "Node"},
		}},
		{GroupVersion: "nvcre.nvidia.com/v1alpha1", APIResources: []metav1.APIResource{
			{Name: "certifications/status", Kind: "Certification", Namespaced: true},
			{Name: "certifications", Kind: "Certification", Namespaced: true},
		}},
	}
	r := DiscoveryResolver{Client: fd}
	for _, tc := range []struct {
		gvk  schema.GroupVersionKind
		want Resolution
	}{
		{schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, podRes},
		{schema.GroupVersionKind{Version: "v1", Kind: "Node"}, nodeRes},
		{schema.GroupVersionKind{Group: "nvcre.nvidia.com", Version: "v1alpha1", Kind: "Certification"}, certRes},
		// The kind is missing from a served group/version.
		{schema.GroupVersionKind{Version: "v1", Kind: "Pods"}, Resolution{}},
		// The group/version is not served at all (404).
		{schema.GroupVersionKind{Group: "trainer.kubeflow.org", Version: "v1alpha1", Kind: "TrainJob"}, Resolution{}},
	} {
		got, err := r.Resolve(context.Background(), tc.gvk)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %+v, %v; want %+v", tc.gvk, got, err, tc.want)
		}
	}

	// Other errors are returned, not read as "not served".
	fd.PrependReactor("get", "resource", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, err503
	})
	if _, err := r.Resolve(context.Background(), schema.GroupVersionKind{Version: "v1", Kind: "Pod"}); err == nil {
		t.Error("expected the 503")
	}
}

// DynamicSource end to end against client-go's fake dynamic client, which
// also records every call: the runner only lists and watches.
func TestDynamicSourceReadOnly(t *testing.T) {
	pods := []runtime.Object{
		&unstructured.Unstructured{Object: pod("a")},
		&unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"namespace": "elsewhere", "name": "b"}}},
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{podRes.Resource: "PodList"}, pods...)
	spec, _ := wait.Parse(podDrain())
	r := &Runner{Source: DynamicSource{Client: client}, Resolver: served(podRes), Clock: clock.RealClock{}}

	done := make(chan Result, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- r.Run(ctx, spec) }()

	// Wait for the watch, then delete the pod in the drained namespace.
	for {
		if acts := client.Actions(); len(acts) >= 2 {
			break
		}
		select {
		case res := <-done:
			t.Fatalf("finished early: %+v", res)
		default:
		}
		time.Sleep(time.Millisecond)
	}
	if err := client.Tracker().Delete(podRes.Resource, "nvcre-certification", "a"); err != nil {
		t.Fatal(err)
	}
	res := <-done
	if res.Verdict != wait.Success {
		t.Fatalf("result %+v", res)
	}
	for _, a := range client.Actions() {
		if v := a.GetVerb(); v != "list" && v != "watch" {
			t.Errorf("runner issued %s", v)
		}
		if a.GetNamespace() != "nvcre-certification" {
			t.Errorf("%s in namespace %q", a.GetVerb(), a.GetNamespace())
		}
	}
}
