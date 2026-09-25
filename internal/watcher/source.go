// Package watcher runs a wait against a cluster: it resolves the kind,
// lists and watches the selected objects, and feeds each snapshot to the
// pure evaluator in internal/wait until the verdict settles or time runs out.
package watcher

import (
	"context"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
)

// Resolution is what discovery says about a kind.
type Resolution struct {
	Served     bool
	Resource   schema.GroupVersionResource
	Namespaced bool
}

// Resolver maps a kind to its resource.
type Resolver interface {
	Resolve(ctx context.Context, gvk schema.GroupVersionKind) (Resolution, error)
}

// Source is the read-only access the runner needs. Nothing else is called:
// the action never creates, patches or deletes.
type Source interface {
	List(ctx context.Context, res Resolution, namespace string, opts metav1.ListOptions) (*unstructured.UnstructuredList, error)
	Watch(ctx context.Context, res Resolution, namespace string, opts metav1.ListOptions) (watch.Interface, error)
}

// DiscoveryResolver asks the API server for the kind's group/version on
// every call, so a CRD established mid-wait is picked up and a stale cache
// never hides one. A group/version the server does not serve (404), or one
// without the kind, resolves to "not served". Any other error, such as the
// 503 of an unavailable aggregated API, is returned for the caller to retry.
type DiscoveryResolver struct {
	Client discovery.DiscoveryInterface
}

func (d DiscoveryResolver) Resolve(ctx context.Context, gvk schema.GroupVersionKind) (Resolution, error) {
	list, err := discovery.ToDiscoveryInterfaceWithContext(d.Client).ServerResourcesForGroupVersionWithContext(ctx, gvk.GroupVersion().String())
	if apierrors.IsNotFound(err) {
		return Resolution{}, nil
	}
	if err != nil {
		return Resolution{}, err
	}
	for _, r := range list.APIResources {
		if r.Kind == gvk.Kind && !strings.Contains(r.Name, "/") {
			return Resolution{
				Served:     true,
				Resource:   gvk.GroupVersion().WithResource(r.Name),
				Namespaced: r.Namespaced,
			}, nil
		}
	}
	return Resolution{}, nil
}

// DynamicSource reads through the dynamic client.
type DynamicSource struct {
	Client dynamic.Interface
}

func (d DynamicSource) resource(res Resolution, namespace string) dynamic.ResourceInterface {
	if res.Namespaced && namespace != "" {
		return d.Client.Resource(res.Resource).Namespace(namespace)
	}
	return d.Client.Resource(res.Resource)
}

func (d DynamicSource) List(ctx context.Context, res Resolution, namespace string, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	return d.resource(res, namespace).List(ctx, opts)
}

func (d DynamicSource) Watch(ctx context.Context, res Resolution, namespace string, opts metav1.ListOptions) (watch.Interface, error) {
	return d.resource(res, namespace).Watch(ctx, opts)
}
