package watcher

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/turfbuild/terraform-provider-kubewait/internal/wait"
)

var t0 = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

// fakeWatcher is a watch whose Stop only records the stop: its channel stays
// readable, so a runner that kept reading a stopped watch would be caught.
type fakeWatcher struct {
	ch      chan watch.Event
	stopped atomic.Bool
}

func (w *fakeWatcher) Stop()                          { w.stopped.Store(true) }
func (w *fakeWatcher) ResultChan() <-chan watch.Event { return w.ch }

// fakeSource is a scripted API server: current objects, queued errors, and
// the watches it handed out.
type fakeSource struct {
	mu        sync.Mutex
	objs      map[string]map[string]any
	listErrs  []error
	watchErrs []error
	watchers  []*fakeWatcher
	lists     int
	rv        int
	listOpts  []metav1.ListOptions
	watchOpts []metav1.ListOptions
}

func newSource(objs ...map[string]any) *fakeSource {
	f := &fakeSource{objs: map[string]map[string]any{}}
	for _, o := range objs {
		f.objs[key(o)] = o
	}
	return f
}

func (f *fakeSource) List(_ context.Context, _ Resolution, _ string, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	f.listOpts = append(f.listOpts, opts)
	if len(f.listErrs) > 0 {
		err := f.listErrs[0]
		f.listErrs = f.listErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	l := &unstructured.UnstructuredList{}
	l.SetResourceVersion(strconv.Itoa(f.rv))
	for _, o := range f.objs {
		l.Items = append(l.Items, unstructured.Unstructured{Object: runtime.DeepCopyJSON(o)})
	}
	return l, nil
}

func (f *fakeSource) Watch(_ context.Context, _ Resolution, _ string, opts metav1.ListOptions) (watch.Interface, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watchOpts = append(f.watchOpts, opts)
	if len(f.watchErrs) > 0 {
		err := f.watchErrs[0]
		f.watchErrs = f.watchErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	w := &fakeWatcher{ch: make(chan watch.Event, 100)}
	f.watchers = append(f.watchers, w)
	return w, nil
}

func (f *fakeSource) current() *fakeWatcher {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.watchers) == 0 {
		return nil
	}
	return f.watchers[len(f.watchers)-1]
}

// set changes server state and returns the event a watch would deliver.
func (f *fakeSource) set(o map[string]any) watch.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rv++
	typ := watch.Modified
	if _, ok := f.objs[key(o)]; !ok {
		typ = watch.Added
	}
	f.objs[key(o)] = o
	return watch.Event{Type: typ, Object: &unstructured.Unstructured{Object: runtime.DeepCopyJSON(o)}}
}

func (f *fakeSource) remove(o map[string]any) watch.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rv++
	delete(f.objs, key(o))
	return watch.Event{Type: watch.Deleted, Object: &unstructured.Unstructured{Object: runtime.DeepCopyJSON(o)}}
}

type fakeResolver struct {
	mu    sync.Mutex
	res   Resolution
	errs  []error
	calls int
}

func (f *fakeResolver) Resolve(context.Context, schema.GroupVersionKind) (Resolution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return Resolution{}, err
		}
	}
	return f.res, nil
}

func (f *fakeResolver) serve(res Resolution) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.res = res
}

var (
	certRes = Resolution{Served: true, Namespaced: true,
		Resource: schema.GroupVersionResource{Group: "nvcre.nvidia.com", Version: "v1alpha1", Resource: "certifications"}}
	podRes  = Resolution{Served: true, Namespaced: true, Resource: schema.GroupVersionResource{Version: "v1", Resource: "pods"}}
	nodeRes = Resolution{Served: true, Resource: schema.GroupVersionResource{Version: "v1", Resource: "nodes"}}
)

// harness runs a Runner against the fakes in lockstep with a fake clock:
// the runner reports each time it is about to block, and when its timer
// fires, so the test knows exactly when it is idle.
type harness struct {
	t        *testing.T
	fc       *clocktesting.FakeClock
	src      *fakeSource
	res      *fakeResolver
	idle     chan time.Time
	done     chan Result
	wake     time.Time
	result   *Result
	endedAt  time.Duration
	cancel   context.CancelFunc
	mu       sync.Mutex
	progress []string
}

func start(t *testing.T, raw wait.Raw, src *fakeSource, res *fakeResolver) *harness {
	t.Helper()
	spec, probs := wait.Parse(raw)
	if spec == nil {
		t.Fatalf("bad spec: %+v", probs)
	}
	h := &harness{t: t, fc: clocktesting.NewFakeClock(t0), src: src, res: res,
		idle: make(chan time.Time), done: make(chan Result, 1)}
	r := &Runner{Source: src, Resolver: res, Clock: h.fc,
		Progress: func(m string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.progress = append(h.progress, fmt.Sprintf("[%s] %s", h.fc.Since(t0), m))
		},
		idle: func(wake time.Time) { h.idle <- wake },
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	t.Cleanup(cancel)
	go func() { h.done <- r.Run(ctx, spec) }()
	h.next()
	return h
}

// next waits for the runner to block again or finish.
func (h *harness) next() {
	h.t.Helper()
	select {
	case h.wake = <-h.idle:
	case r := <-h.done:
		h.result, h.endedAt = &r, h.fc.Since(t0)
	case <-time.After(10 * time.Second):
		h.t.Fatal("runner neither blocked nor finished")
	}
}

// advance moves the clock forward by d, waking the runner at each of its
// timers on the way.
func (h *harness) advance(d time.Duration) {
	h.t.Helper()
	target := h.fc.Now().Add(d)
	for h.result == nil {
		now := h.fc.Now()
		if !now.Before(target) {
			return
		}
		if h.wake.After(target) {
			h.fc.Step(target.Sub(now))
			return
		}
		h.fc.Step(h.wake.Sub(now))
		h.next()
	}
}

// send delivers a watch event and waits for the runner to process it.
func (h *harness) send(ev watch.Event) {
	h.t.Helper()
	w := h.src.current()
	if w == nil {
		h.t.Fatal("no watch open")
	}
	w.ch <- ev
	h.next()
}

func (h *harness) finish() Result {
	h.t.Helper()
	h.advance(24 * time.Hour)
	if h.result == nil {
		h.t.Fatal("runner did not finish")
	}
	return *h.result
}

func (h *harness) running() {
	h.t.Helper()
	if h.result != nil {
		h.t.Fatalf("finished early at %s: %+v", h.endedAt, *h.result)
	}
}

func (h *harness) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.progress...)
}

func (h *harness) progressContains(sub string) bool {
	for _, m := range h.messages() {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}
