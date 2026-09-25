// Package integration runs waits against a real kube-apiserver and etcd
// (controller-runtime's envtest). Tests skip unless KUBEBUILDER_ASSETS
// points at the binaries: `make testint`.
package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/turfbuild/terraform-provider-kubewait/internal/wait"
	"github.com/turfbuild/terraform-provider-kubewait/internal/watcher"
)

var (
	env *envtest.Environment
	// admin drives the fixtures. reader runs every wait: it is bound to a
	// ClusterRole with get, list and watch only, so a wait that tried to
	// mutate anything would fail. unbound has no role at all.
	admin, reader, unbound *rest.Config
	kube                   kubernetes.Interface
	dyn                    dynamic.Interface
	startErr               error
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") != "" {
		startErr = start()
	}
	code := m.Run()
	if env != nil {
		_ = env.Stop()
	}
	os.Exit(code)
}

func start() error {
	env = &envtest.Environment{
		CRDDirectoryPaths:     []string{"testdata/crds"},
		ErrorIfCRDPathMissing: true,
	}
	var err error
	if admin, err = env.Start(); err != nil {
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
	r, err := env.AddUser(envtest.User{Name: "kubewait-reader"}, admin)
	if err != nil {
		return err
	}
	reader = r.Config()
	u, err := env.AddUser(envtest.User{Name: "kubewait-unbound"}, admin)
	if err != nil {
		return err
	}
	unbound = u.Config()
	return nil
}

func requireEnv(t *testing.T) {
	t.Helper()
	if env == nil {
		t.Skip("KUBEBUILDER_ASSETS is not set; run `make testint`")
	}
	if startErr != nil {
		t.Fatalf("envtest failed to start: %v", startErr)
	}
}

var nsSeq atomic.Int64

// namespace creates a fresh namespace. envtest has no namespace controller,
// so namespaces are never deleted; each test gets its own.
func namespace(t *testing.T, prefix string) string {
	t.Helper()
	name := fmt.Sprintf("%s-%d", prefix, nsSeq.Add(1))
	if _, err := kube.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return name
}

// running is one wait in flight, run as the given user.
type running struct {
	t        *testing.T
	start    time.Time
	done     chan watcher.Result
	result   *watcher.Result
	took     time.Duration
	mu       sync.Mutex
	progress []string
}

func startWait(t *testing.T, raw wait.Raw, cfg *rest.Config) *running {
	t.Helper()
	spec, probs := wait.Parse(raw)
	if spec == nil {
		t.Fatalf("invalid wait: %+v", probs)
	}
	w := &running{t: t, start: time.Now(), done: make(chan watcher.Result, 1)}
	r := &watcher.Runner{
		Source:   watcher.DynamicSource{Client: dynamic.NewForConfigOrDie(cfg)},
		Resolver: watcher.DiscoveryResolver{Client: discovery.NewDiscoveryClientForConfigOrDie(cfg)},
		Clock:    clock.RealClock{},
		Progress: func(m string) {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.progress = append(w.progress, m)
			t.Logf("progress +%s: %s", time.Since(w.start).Round(time.Millisecond), m)
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { w.done <- r.Run(ctx, spec) }()
	return w
}

// awaitProgress blocks until a progress message contains every substring.
func (w *running) awaitProgress(subs ...string) string {
	w.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		for _, m := range w.progress {
			ok := true
			for _, s := range subs {
				ok = ok && strings.Contains(m, s)
			}
			if ok {
				w.mu.Unlock()
				return m
			}
		}
		w.mu.Unlock()
		select {
		case r := <-w.done:
			w.result, w.took = &r, time.Since(w.start)
			w.t.Fatalf("finished (%+v) before progress %q", r, subs)
		case <-time.After(50 * time.Millisecond):
		}
	}
	w.t.Fatalf("no progress containing %q", subs)
	return ""
}

// still asserts the wait has not finished after d.
func (w *running) still(d time.Duration) {
	w.t.Helper()
	select {
	case r := <-w.done:
		w.t.Fatalf("finished early after %s: %+v", time.Since(w.start), r)
	case <-time.After(d):
	}
}

func (w *running) finish() (watcher.Result, time.Duration) {
	w.t.Helper()
	if w.result != nil {
		return *w.result, w.took
	}
	select {
	case r := <-w.done:
		w.result, w.took = &r, time.Since(w.start)
		return r, w.took
	case <-time.After(2 * time.Minute):
		w.t.Fatal("wait did not finish")
	}
	return watcher.Result{}, 0
}

func (w *running) expectSuccess() watcher.Result {
	w.t.Helper()
	r, took := w.finish()
	if r.Verdict != wait.Success {
		w.t.Fatalf("after %s: %s\n%s", took, r.Summary, r.Detail)
	}
	return r
}

func (w *running) expectFailure(summary string, details ...string) watcher.Result {
	w.t.Helper()
	r, took := w.finish()
	if r.Verdict != wait.Failure || r.Summary != summary {
		w.t.Fatalf("after %s: got %s %q, want failure %q\n%s", took, r.Verdict, r.Summary, summary, r.Detail)
	}
	for _, d := range details {
		if !strings.Contains(r.Detail, d) {
			w.t.Errorf("detail does not contain %q:\n%s", d, r.Detail)
		}
	}
	return r
}

// retry runs f until it succeeds, for writes that race with apiserver
// bootstrap (the first Service create) or with conflicts.
func retry(t *testing.T, f func() error) {
	t.Helper()
	var err error
	for i := 0; i < 50; i++ {
		if err = f(); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal(err)
}
