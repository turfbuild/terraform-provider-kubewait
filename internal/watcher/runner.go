package watcher

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/clock"

	"github.com/turfbuild/terraform-provider-kubewait/internal/wait"
)

const (
	// minBackoff is the first retry delay after an API error; it doubles up
	// to poll_interval.
	minBackoff = time.Second
	// minCycle spaces out re-lists when a watch keeps closing at once.
	minCycle = time.Second
	// requestTimeout bounds one discovery or list request.
	requestTimeout = time.Minute
)

// Runner runs waits.
type Runner struct {
	Source   Source
	Resolver Resolver
	Clock    clock.WithTicker
	// Progress receives each progress message. It is called only on the
	// goroutine running Run, and never after Run returns.
	Progress func(msg string)

	// idle, when set, is called just before the runner blocks, with the time
	// its timer fires. Tests use it to drive the fake clock in lockstep.
	idle func(wake time.Time)
}

// Result is how a wait ended.
type Result struct {
	// Verdict is Success only for a settled success.
	Verdict wait.Verdict
	// Summary and Detail describe a failure; empty on success.
	Summary, Detail string
	// Warnings accompany a success, such as an unserved kind.
	Warnings  []string
	TimedOut  bool
	Cancelled bool
}

type noteKind int

const (
	noNote noteKind = iota
	apiErrorNote
	notServedNote
)

type run struct {
	*Runner
	s        *wait.Spec
	start    time.Time
	deadline time.Time
	tracker  *wait.Tracker
	cache    map[string]map[string]any
	last     wait.Outcome

	note     string
	noteKind noteKind

	emitted  bool
	lastKey  string
	lastEmit time.Time

	unauthRetried bool
	backoff       time.Duration
}

// Run waits until the spec's verdict settles, the timeout expires, a
// non-retryable error occurs, or ctx is cancelled.
func (r *Runner) Run(ctx context.Context, s *wait.Spec) Result {
	now := r.Clock.Now()
	x := &run{Runner: r, s: s, start: now, deadline: now.Add(s.Timeout), tracker: wait.NewTracker(s.Settle)}
	return x.loop(ctx)
}

func (x *run) loop(ctx context.Context) Result {
	for {
		if ctx.Err() != nil {
			return x.cancelled()
		}
		if !x.Clock.Now().Before(x.deadline) {
			return x.timeout()
		}
		cycleStart := x.Clock.Now()

		// Resolve and list. Stale watch events can't reach this cache: the
		// previous cycle's watch was stopped before we got here.
		res, err := x.resolve(ctx)
		var list *unstructured.UnstructuredList
		if err == nil && res.Served {
			if r, bad := x.scopeError(res); bad {
				return r
			}
			list, err = x.list(ctx, res)
		}
		if err != nil {
			if r, done := x.apiError(ctx, err); done {
				return r
			}
			continue
		}
		x.unauthRetried, x.backoff = false, 0
		x.cache = map[string]map[string]any{}
		if res.Served {
			x.setNote(noNote, "")
			for i := range list.Items {
				o := list.Items[i].Object
				x.cache[key(o)] = o
			}
		} else {
			x.setNote(notServedNote, fmt.Sprintf("kind %s not served (treated as no objects)", x.kindString()))
		}
		if r, done := x.observe(x.evaluate()); done {
			return r
		}

		// Watch until the next resync.
		var w watch.Interface
		if res.Served && x.s.Watch {
			opts := x.listOptions()
			opts.ResourceVersion = list.GetResourceVersion()
			opts.AllowWatchBookmarks = true
			w, err = x.Source.Watch(ctx, res, x.s.Namespace, opts)
			if err != nil {
				if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
					continue
				}
				if r, done := x.apiError(ctx, err); done {
					return r
				}
				continue
			}
		}
		r, done := x.waitPhase(ctx, w, cycleStart)
		if w != nil {
			w.Stop()
		}
		if done {
			return r
		}
	}
}

// waitPhase applies watch events until it is time to resync. done means
// the wait is over.
func (x *run) waitPhase(ctx context.Context, w watch.Interface, cycleStart time.Time) (Result, bool) {
	resyncAt := x.Clock.Now().Add(x.s.PollInterval)
	var events <-chan watch.Event
	if w != nil {
		events = w.ResultChan()
	}
	for {
		now := x.Clock.Now()
		if !now.Before(x.deadline) {
			return x.timeout(), true
		}
		x.heartbeat(now)
		if dl, ok := x.tracker.Deadline(); ok && !now.Before(dl) {
			if w == nil {
				// Polling: settle on a fresh observation.
				return Result{}, false
			}
			// Watching: no event since, so the verdict has held.
			if r, done := x.observe(x.last); done {
				return r, true
			}
		}
		if !now.Before(resyncAt) {
			return Result{}, false
		}

		next := earliest(resyncAt, x.deadline, x.nextHeartbeat())
		if dl, ok := x.tracker.Deadline(); ok && dl.After(now) {
			next = earliest(next, dl)
		}
		t := x.Clock.NewTimer(next.Sub(now))
		if x.idle != nil {
			x.idle(next)
		}
		select {
		case <-ctx.Done():
			t.Stop()
			return x.cancelled(), true
		case <-t.C():
		case ev, ok := <-events:
			t.Stop()
			if !ok {
				// The server closed the watch: re-list now, not an error.
				if since := x.Clock.Now().Sub(cycleStart); since < minCycle {
					if r, done := x.sleep(ctx, minCycle-since); done {
						return r, true
					}
				}
				return Result{}, false
			}
			switch ev.Type {
			case watch.Bookmark:
				continue
			case watch.Error:
				err := apierrors.FromObject(ev.Object)
				if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
					return Result{}, false
				}
				r, done := x.apiError(ctx, err)
				return r, done
			case watch.Added, watch.Modified:
				if u, ok := ev.Object.(*unstructured.Unstructured); ok {
					x.cache[key(u.Object)] = u.Object
				}
			case watch.Deleted:
				if u, ok := ev.Object.(*unstructured.Unstructured); ok {
					delete(x.cache, key(u.Object))
				}
			}
			if r, done := x.observe(x.evaluate()); done {
				return r, true
			}
		}
	}
}

// apiError applies the error policy. 403 ends the wait at once. 401 gets
// one immediate retry, since client-go's exec credential plugin refreshes
// only on the request after a 401, then ends it. Anything else is retried
// with backoff until timeout, and counts as a pending observation: an
// interval nobody observed cannot count toward "held continuously".
func (x *run) apiError(ctx context.Context, err error) (Result, bool) {
	switch {
	case apierrors.IsForbidden(err):
		return x.fatal("kubewait_condition cannot read "+x.kindString(),
			fmt.Sprintf("The API server refused access (403 Forbidden): %s\n\nThe wait fails at once: waiting will not grant permission. The provider's credentials need get, list and watch on %s.", err, x.kindString())), true
	case apierrors.IsUnauthorized(err):
		if !x.unauthRetried {
			x.unauthRetried = true
			return Result{}, false
		}
		return x.fatal("kubewait_condition is not authenticated",
			fmt.Sprintf("The API server rejected the credentials (401 Unauthorized) twice in a row, the second time after a credential refresh: %s\n\nThe wait fails at once: waiting will not fix it. Check the provider's token, client certificate or exec plugin.", err)), true
	}
	x.setNote(apiErrorNote, "API error, retrying: "+err.Error())
	if r, done := x.observe(wait.Outcome{Verdict: wait.Pending, Reason: "cannot observe " + x.kindString()}); done {
		return r, true
	}
	if x.backoff == 0 {
		x.backoff = minBackoff
	} else {
		x.backoff *= 2
	}
	if x.backoff > x.s.PollInterval {
		x.backoff = x.s.PollInterval
	}
	return x.sleep(ctx, x.backoff)
}

// sleep waits for d, still honouring the deadline, heartbeats and ctx.
func (x *run) sleep(ctx context.Context, d time.Duration) (Result, bool) {
	until := x.Clock.Now().Add(d)
	for {
		now := x.Clock.Now()
		if !now.Before(x.deadline) {
			return x.timeout(), true
		}
		x.heartbeat(now)
		if !now.Before(until) {
			return Result{}, false
		}
		next := earliest(until, x.deadline, x.nextHeartbeat())
		t := x.Clock.NewTimer(next.Sub(now))
		if x.idle != nil {
			x.idle(next)
		}
		select {
		case <-ctx.Done():
			t.Stop()
			return x.cancelled(), true
		case <-t.C():
		}
	}
}

func (x *run) resolve(ctx context.Context) (Resolution, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	return x.Resolver.Resolve(ctx, x.s.GVK)
}

func (x *run) list(ctx context.Context, res Resolution) (*unstructured.UnstructuredList, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	return x.Source.List(ctx, res, x.s.Namespace, x.listOptions())
}

func (x *run) listOptions() metav1.ListOptions {
	if x.s.Single() {
		return metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", x.s.Name).String()}
	}
	return metav1.ListOptions{LabelSelector: x.s.LabelSelector, FieldSelector: x.s.FieldSelector}
}

func (x *run) scopeError(res Resolution) (Result, bool) {
	switch {
	case !res.Namespaced && x.s.Namespace != "":
		return x.fatal("Invalid kubewait_condition configuration",
			fmt.Sprintf("%s is cluster-scoped, so namespace must not be set (got %q).", x.kindString(), x.s.Namespace)), true
	case res.Namespaced && x.s.Single() && x.s.Namespace == "":
		return x.fatal("Invalid kubewait_condition configuration",
			fmt.Sprintf("%s is namespaced, so single-object mode (name = %q) needs namespace. There is no implicit default namespace.", x.kindString(), x.s.Name)), true
	}
	return Result{}, false
}

func (x *run) evaluate() wait.Outcome {
	objs := make([]map[string]any, 0, len(x.cache))
	for _, o := range x.cache {
		objs = append(objs, o)
	}
	return wait.Evaluate(x.s, objs)
}

// observe feeds one outcome to the settle clock and emits progress when the
// verdict (or the error/unserved state) changes.
func (x *run) observe(out wait.Outcome) (Result, bool) {
	now := x.Clock.Now()
	x.last = out
	settled, _ := x.tracker.Observe(now, out.Verdict)
	if x.stateKey() != x.lastKey {
		x.emit(now)
	}
	if !settled {
		return Result{}, false
	}
	x.emitFinal(now)
	if out.Verdict == wait.Success {
		r := Result{Verdict: wait.Success}
		if x.noteKind == notServedNote {
			r.Warnings = append(r.Warnings, fmt.Sprintf(
				"The API server does not serve %s, so the wait observed no objects and succeeded. If the kind is misspelled or its CRD is missing, this success is vacuous.", x.kindString()))
		}
		return r, true
	}
	return Result{Verdict: wait.Failure, Summary: "kubewait_condition failed",
		Detail: fmt.Sprintf("The wait on %s reached a failure verdict that held for settle (%s).\n\n%s",
			x.kindString(), wait.FormatDuration(x.s.Settle), x.message(now))}, true
}

func (x *run) timeout() Result {
	now := x.Clock.Now()
	x.emitFinal(now)
	return Result{Verdict: wait.Failure, TimedOut: true, Summary: "kubewait_condition timed out",
		Detail: fmt.Sprintf("No verdict on %s settled within timeout (%s). Last observation:\n\n%s",
			x.kindString(), wait.FormatDuration(x.s.Timeout), x.message(now))}
}

func (x *run) fatal(summary, detail string) Result {
	x.last = wait.Outcome{Verdict: wait.Failure, Reason: summary}
	x.setNote(noNote, "")
	x.emitFinal(x.Clock.Now())
	return Result{Verdict: wait.Failure, Summary: summary, Detail: detail}
}

func (x *run) cancelled() Result {
	detail := "The wait was cancelled before a verdict settled."
	if x.emitted {
		detail += " Last observation:\n\n" + x.message(x.Clock.Now())
	}
	return Result{Verdict: wait.Failure, Cancelled: true, Summary: "kubewait_condition cancelled", Detail: detail}
}

func (x *run) setNote(k noteKind, note string) { x.noteKind, x.note = k, note }

func (x *run) stateKey() string { return fmt.Sprintf("%s|%d", x.last.Verdict, x.noteKind) }

func (x *run) message(now time.Time) string {
	snap := wait.Snapshot{
		Outcome:   x.last,
		Elapsed:   now.Sub(x.start),
		Remaining: x.deadline.Sub(now),
		Settle:    x.s.Settle,
		Note:      x.note,
	}
	if x.tracker.State() != wait.Pending {
		snap.Settling = x.tracker.Held(now)
	}
	return wait.FormatProgress(x.s, snap)
}

func (x *run) emit(now time.Time) {
	x.emitted, x.lastKey, x.lastEmit = true, x.stateKey(), now
	if x.Progress != nil {
		x.Progress(x.message(now))
	}
}

// emitFinal sends the closing progress event unless the last one already
// said the same thing at the same instant.
func (x *run) emitFinal(now time.Time) {
	if x.emitted && x.lastKey == x.stateKey() && x.lastEmit.Equal(now) {
		return
	}
	x.emit(now)
}

func (x *run) heartbeat(now time.Time) {
	if x.emitted && now.Sub(x.lastEmit) >= x.s.ProgressInterval {
		x.emit(now)
	}
}

func (x *run) nextHeartbeat() time.Time {
	if !x.emitted {
		return x.deadline
	}
	return x.lastEmit.Add(x.s.ProgressInterval)
}

func (x *run) kindString() string {
	if x.s.GVK.Group == "" {
		return x.s.GVK.Version + " " + x.s.GVK.Kind
	}
	return x.s.GVK.Group + "/" + x.s.GVK.Version + " " + x.s.GVK.Kind
}

func key(o map[string]any) string { return wait.RefOf(o).String() }

func earliest(ts ...time.Time) time.Time {
	e := ts[0]
	for _, t := range ts[1:] {
		if t.Before(e) {
			e = t
		}
	}
	return e
}
