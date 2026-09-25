package watcher

import (
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/turfbuild/terraform-provider-kubewait/internal/fixtures"
	"github.com/turfbuild/terraform-provider-kubewait/internal/wait"
)

func cert(conds ...string) map[string]any {
	var cs []any
	for _, c := range conds {
		typ, st, _ := strings.Cut(c, "=")
		cs = append(cs, map[string]any{"type": typ, "status": st, "reason": "R"})
	}
	return map[string]any{
		"apiVersion": "nvcre.nvidia.com/v1alpha1", "kind": "Certification",
		"metadata": map[string]any{"namespace": "nvcre-certification", "name": "gpu-pools"},
		"status":   map[string]any{"conditions": cs},
	}
}

func pod(name string) map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"namespace": "nvcre-certification", "name": name}}
}

func readyNode(name string, ready bool) map[string]any {
	st := "False"
	if ready {
		st = "True"
	}
	return map[string]any{"apiVersion": "v1", "kind": "Node", "metadata": map[string]any{"name": name},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": st}}}}
}

func certWait(settle string) wait.Raw {
	return fixtures.CertificationTerminal("nvcre-certification", "gpu-pools", "60m", settle)
}

func podDrain() wait.Raw { return fixtures.PodsDrained("nvcre-certification", "10m") }

func served(r Resolution) *fakeResolver { return &fakeResolver{res: r} }

var (
	err500 = apierrors.NewInternalError(errors.New("etcd leader changed"))
	err503 = apierrors.NewServiceUnavailable("the server is currently unable to handle the request")
	err403 = apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New(`User "kubewait" cannot list resource "pods"`))
	err401 = apierrors.NewUnauthorized("Unauthorized")
)

func TestWatchDrivenSuccess(t *testing.T) {
	src := newSource(cert("Succeeded=False", "Failed=False"))
	h := start(t, certWait("0s"), src, served(certRes))
	h.advance(5 * time.Minute)
	h.running()
	h.send(src.set(cert("Succeeded=True", "Failed=False")))
	if h.result == nil || h.result.Verdict != wait.Success || h.endedAt != 5*time.Minute {
		t.Fatalf("result %+v at %s", h.result, h.endedAt)
	}
	// Single mode lists and watches by name.
	if got := src.listOpts[0].FieldSelector; got != "metadata.name=gpu-pools" {
		t.Errorf("field selector %q", got)
	}
	if o := src.watchOpts[0]; o.ResourceVersion != "0" || !o.AllowWatchBookmarks {
		t.Errorf("watch opts %+v", o)
	}
}

func TestWatchDrivenFailure(t *testing.T) {
	src := newSource(cert("Succeeded=False", "Failed=False"))
	h := start(t, certWait("0s"), src, served(certRes))
	h.send(src.set(cert("Succeeded=False", "Failed=True")))
	r := h.result
	if r == nil || r.Verdict != wait.Failure || r.Summary != "kubewait_condition failed" || !strings.Contains(r.Detail, "Failed=True (R)") {
		t.Fatalf("result %+v", r)
	}
}

func TestSettleOnWatch(t *testing.T) {
	src := newSource(cert("Succeeded=False"))
	h := start(t, certWait("2m"), src, served(certRes))
	h.advance(time.Minute)
	h.send(src.set(cert("Succeeded=True")))
	h.advance(119 * time.Second)
	h.running()
	r := h.finish()
	if r.Verdict != wait.Success || h.endedAt != 3*time.Minute {
		t.Fatalf("result %+v at %s, want success at 3m (1m + settle 2m)", r, h.endedAt)
	}
}

// The NVCRE repeatCount > 1 flip, end to end through the runner.
func TestSettleFlipOnWatch(t *testing.T) {
	src := newSource(cert("Succeeded=False", "Failed=False"))
	h := start(t, certWait("2m"), src, served(certRes))
	h.advance(10 * time.Minute)
	h.send(src.set(cert("Succeeded=False", "Failed=True")))
	h.advance(90 * time.Second)
	h.send(src.set(cert("Succeeded=False", "Failed=False"))) // back to InProgress
	h.advance(10 * time.Minute)
	h.running()
	h.send(src.set(cert("Succeeded=True", "Failed=False")))
	r := h.finish()
	if r.Verdict != wait.Success || h.endedAt != 23*time.Minute+30*time.Second {
		t.Fatalf("result %+v at %s", r, h.endedAt)
	}
}

func TestResyncAfterMissedEvent(t *testing.T) {
	src := newSource(pod("a"))
	h := start(t, podDrain(), src, served(podRes))
	src.remove(pod("a")) // the watch never delivers it
	h.advance(9 * time.Second)
	h.running()
	r := h.finish()
	if r.Verdict != wait.Success || h.endedAt != 10*time.Second {
		t.Fatalf("result %+v at %s, want success at the 10s resync", r, h.endedAt)
	}
	if !src.watchers[0].stopped.Load() {
		t.Error("first watch not stopped")
	}
}

func TestPollOnly(t *testing.T) {
	raw := podDrain()
	raw.Watch = fixtures.B(false)
	raw.PollInterval = fixtures.S("30s")
	src := newSource(pod("a"))
	h := start(t, raw, src, served(podRes))
	h.advance(45 * time.Second)
	src.remove(pod("a"))
	r := h.finish()
	if r.Verdict != wait.Success || h.endedAt != time.Minute {
		t.Fatalf("result %+v at %s", r, h.endedAt)
	}
	if len(src.watchOpts) != 0 {
		t.Errorf("watch = false opened %d watches", len(src.watchOpts))
	}
	if src.lists != 3 {
		t.Errorf("lists = %d, want 3 (0s, 30s, 60s)", src.lists)
	}
}

// Found in review: after a resync, a still-open old watch could deliver
// stale events into the fresh cache and settle a false verdict.
func TestResyncIgnoresStaleWatch(t *testing.T) {
	raw := wait.Raw{APIVersion: fixtures.S("v1"), Kind: fixtures.S("Node"), Timeout: fixtures.S("2m"),
		SuccessConditions: fixtures.Cs(fixtures.C("Ready", "True"))}
	src := newSource(readyNode("a", false))
	h := start(t, raw, src, served(nodeRes))
	old := src.current()
	h.advance(10 * time.Second) // resync
	if !old.stopped.Load() || src.current() == old {
		t.Fatal("resync did not replace the watch")
	}
	// A stale event on the old watch, showing a ready node that no longer
	// exists, must never be read.
	old.ch <- src.set(readyNode("a", true))
	src.remove(readyNode("a", true))
	h.advance(time.Minute)
	h.running()
	if len(old.ch) != 1 {
		t.Error("stale event was consumed from a stopped watch")
	}
}

func TestBookmarkAndWatchClose(t *testing.T) {
	src := newSource(pod("a"))
	h := start(t, podDrain(), src, served(podRes))
	h.send(watch.Event{Type: watch.Bookmark, Object: &metav1.Status{}})
	h.running()
	// The server closes the watch: re-list (not an error, no settle reset).
	src.remove(pod("a"))
	close(src.current().ch)
	h.next()
	h.advance(time.Second)
	if h.result == nil || h.result.Verdict != wait.Success {
		t.Fatalf("result %+v", h.result)
	}
	if h.progressContains("API error") {
		t.Error("watch close reported as an API error")
	}
}

func TestWatchGoneRelists(t *testing.T) {
	src := newSource(pod("a"))
	h := start(t, podDrain(), src, served(podRes))
	src.remove(pod("a"))
	gone := apierrors.NewResourceExpired("too old resource version")
	h.send(watch.Event{Type: watch.Error, Object: &gone.ErrStatus})
	if h.result == nil || h.result.Verdict != wait.Success || h.endedAt != 0 {
		t.Fatalf("result %+v at %s", h.result, h.endedAt)
	}
}

// A retried error counts as pending, so it resets settle.
func TestAPIErrorRetriedAndResetsSettle(t *testing.T) {
	src := newSource(cert("Succeeded=True"))
	h := start(t, certWait("1m"), src, served(certRes))
	h.advance(30 * time.Second) // success held 30s
	e := err500.ErrStatus
	h.send(watch.Event{Type: watch.Error, Object: &e})
	if !h.progressContains("pending: cannot observe nvcre.nvidia.com/v1alpha1 Certification [API error, retrying:") {
		t.Errorf("no error progress:\n%s", strings.Join(h.messages(), "\n"))
	}
	r := h.finish()
	// Error at 30s, backoff 1s, success observed again at 31s, settled at 91s.
	if r.Verdict != wait.Success || h.endedAt != 91*time.Second {
		t.Fatalf("result %+v at %s, want success at 1m31s", r, h.endedAt)
	}
}

func TestListErrorsBackOff(t *testing.T) {
	src := newSource()
	src.listErrs = []error{err500, err503, err500}
	h := start(t, podDrain(), src, served(podRes))
	r := h.finish()
	// 1s + 2s + 4s of backoff.
	if r.Verdict != wait.Success || h.endedAt != 7*time.Second {
		t.Fatalf("result %+v at %s", r, h.endedAt)
	}
	msgs := h.messages()
	if len(msgs) != 2 || !strings.Contains(msgs[0], "API error") || !strings.HasPrefix(msgs[1], "[7s] success: no objects match") {
		t.Errorf("progress: first error and recovery only, got:\n%s", strings.Join(msgs, "\n"))
	}
}

func TestBackoffCappedAtPollInterval(t *testing.T) {
	src := newSource()
	for i := 0; i < 8; i++ {
		src.listErrs = append(src.listErrs, err500)
	}
	raw := podDrain()
	raw.PollInterval = fixtures.S("5s")
	h := start(t, raw, src, served(podRes))
	h.finish()
	// 1 + 2 + 4 + 5 + 5 + 5 + 5 + 5.
	if h.endedAt != 32*time.Second {
		t.Errorf("ended at %s", h.endedAt)
	}
}

func TestForbiddenFailsAtOnce(t *testing.T) {
	src := newSource()
	src.listErrs = []error{err403}
	h := start(t, podDrain(), src, served(podRes))
	r := h.result
	if r == nil || r.Verdict != wait.Failure || h.endedAt != 0 || !strings.Contains(r.Summary, "cannot read v1 Pod") ||
		!strings.Contains(r.Detail, "403 Forbidden") {
		t.Fatalf("result %+v at %s", r, h.endedAt)
	}
	// On discovery too.
	h = start(t, podDrain(), newSource(), &fakeResolver{res: podRes, errs: []error{err403}})
	if h.result == nil || h.endedAt != 0 || !strings.Contains(h.result.Detail, "403") {
		t.Fatalf("result %+v", h.result)
	}
}

func TestUnauthorizedRetriedOnce(t *testing.T) {
	src := newSource()
	src.listErrs = []error{err401}
	h := start(t, podDrain(), src, served(podRes))
	if h.result == nil || h.result.Verdict != wait.Success || h.endedAt != 0 {
		t.Fatalf("a single 401 should be retried at once: %+v at %s", h.result, h.endedAt)
	}

	src = newSource()
	src.listErrs = []error{err401, err401}
	h = start(t, podDrain(), src, served(podRes))
	if h.result == nil || h.result.Verdict != wait.Failure || h.endedAt != 0 || h.result.Summary != "kubewait_condition is not authenticated" {
		t.Fatalf("a repeated 401 should fail at once: %+v at %s", h.result, h.endedAt)
	}

	// A 401 later on is retried once again: success resets the allowance.
	src = newSource(pod("a"))
	h = start(t, podDrain(), src, served(podRes))
	src.listErrs = []error{err401}
	h.advance(10 * time.Second)
	h.running()
	src.listErrs = []error{err401}
	h.advance(10 * time.Second)
	h.running()
}

func TestUnservedKind(t *testing.T) {
	// A drain on a kind nobody serves passes, with a warning.
	h := start(t, fixtures.TrainJobsDrained("nvcre-certification", "10m"), newSource(), &fakeResolver{})
	r := h.result
	if r == nil || r.Verdict != wait.Success || len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "trainer.kubeflow.org/v1alpha1 TrainJob") {
		t.Fatalf("result %+v", r)
	}
	if !h.progressContains("[kind trainer.kubeflow.org/v1alpha1 TrainJob not served (treated as no objects)]") {
		t.Errorf("progress: %v", h.messages())
	}

	// Single mode applies absent.
	h = start(t, fixtures.TrainJobFinished("kubeflow", "pytorch-mnist", "20m"), newSource(), &fakeResolver{})
	if h.result == nil || h.result.Verdict != wait.Failure || !strings.Contains(h.result.Detail, "not found (absent = failure)") {
		t.Fatalf("result %+v", h.result)
	}

	// A success wait stays pending until the CRD is established and an
	// object appears.
	raw := fixtures.CertificationTerminal("nvcre-certification", "gpu-pools", "60m", "0s")
	raw.Absent = wait.Str{}
	res := &fakeResolver{}
	src := newSource()
	h = start(t, raw, src, res)
	h.advance(5 * time.Minute)
	h.running()
	res.serve(certRes)
	src.set(cert("Succeeded=True"))
	h.advance(30 * time.Second)
	if h.result == nil || h.result.Verdict != wait.Success || len(h.result.Warnings) != 0 {
		t.Fatalf("result %+v", h.result)
	}
}

// A discovery error is retried, never read as "not served".
func TestDiscoveryErrorRetried(t *testing.T) {
	res := &fakeResolver{errs: []error{err503, err503}}
	h := start(t, fixtures.TrainJobsDrained("nvcre-certification", "10m"), newSource(), res)
	h.running()
	r := h.finish()
	if r.Verdict != wait.Success || h.endedAt != 3*time.Second {
		t.Fatalf("result %+v at %s", r, h.endedAt)
	}
}

func TestTimeoutReportsUnsettledSuccess(t *testing.T) {
	src := newSource(cert("Succeeded=False"))
	raw := certWait("2m")
	raw.Timeout = fixtures.S("3m")
	h := start(t, raw, src, served(certRes))
	h.advance(90 * time.Second)
	h.send(src.set(cert("Succeeded=True")))
	r := h.finish()
	if !r.TimedOut || r.Verdict != wait.Failure || h.endedAt != 3*time.Minute ||
		!strings.Contains(r.Detail, "success held 1m30s of 2m") || !strings.Contains(r.Detail, "within timeout (3m)") {
		t.Fatalf("result %+v at %s", r, h.endedAt)
	}
}

func TestProgressCadence(t *testing.T) {
	src := newSource(cert("Succeeded=False"))
	raw := certWait("0s")
	raw.ProgressInterval = fixtures.S("1m")
	h := start(t, raw, src, served(certRes))
	h.advance(150 * time.Second)
	h.send(src.set(cert("Succeeded=False", "Other=True"))) // no verdict change: no event
	h.advance(15 * time.Second)
	h.send(src.set(cert("Succeeded=True")))
	var stamps []string
	for _, m := range h.messages() {
		stamps = append(stamps, m[:strings.Index(m, " ")])
	}
	// Start, two heartbeats, the success (which is also the final event).
	if got := strings.Join(stamps, " "); got != "[0s] [1m0s] [2m0s] [2m45s]" {
		t.Errorf("progress at %s\n%s", got, strings.Join(h.messages(), "\n"))
	}
}

func TestCancel(t *testing.T) {
	h := start(t, certWait("0s"), newSource(cert("Succeeded=False")), served(certRes))
	h.cancel()
	h.next()
	if h.result == nil || !h.result.Cancelled || h.result.Verdict == wait.Success {
		t.Fatalf("result %+v", h.result)
	}
}

func TestScopeErrors(t *testing.T) {
	raw := fixtures.Census("nodeGroup=gpu-worker", 2, []string{"a", "b"})
	raw.Namespace = fixtures.S("default")
	h := start(t, raw, newSource(), served(nodeRes))
	if h.result == nil || !strings.Contains(h.result.Detail, "v1 Node is cluster-scoped, so namespace must not be set") {
		t.Fatalf("result %+v", h.result)
	}
	raw = fixtures.CertificationTerminal("x", "gpu-pools", "60m", "0s")
	raw.Namespace = wait.Str{}
	h = start(t, raw, newSource(), served(certRes))
	if h.result == nil || !strings.Contains(h.result.Detail, "is namespaced, so single-object mode") {
		t.Fatalf("result %+v", h.result)
	}
}

func TestSetModeListOptions(t *testing.T) {
	src := newSource()
	raw := fixtures.Census("nodeGroup=gpu-worker", 2, []string{"a", "b"})
	raw.FieldSelector = fixtures.S("spec.unschedulable=false")
	start(t, raw, src, served(nodeRes))
	if o := src.listOpts[0]; o.LabelSelector != "nodeGroup=gpu-worker" || o.FieldSelector != "spec.unschedulable=false" {
		t.Errorf("list opts %+v", o)
	}
}
