package wait

import (
	"strings"
	"testing"
)

func single(mod func(r *Raw)) Raw {
	r := Raw{APIVersion: S("nvcre.nvidia.com/v1alpha1"), Kind: S("Certification"),
		Namespace: S("ns"), Name: S("c"), Timeout: S("60m"),
		SuccessConditions: Cs(C("Succeeded", "True")), FailureConditions: Cs(C("Failed", "True"))}
	if mod != nil {
		mod(&r)
	}
	return r
}

func expect(t *testing.T, out Outcome, v Verdict, reason string) {
	t.Helper()
	if out.Verdict != v {
		t.Errorf("verdict = %s, want %s (reason %q)", out.Verdict, v, out.Reason)
	}
	if reason != "" && !strings.Contains(out.Reason, reason) {
		t.Errorf("reason %q does not contain %q", out.Reason, reason)
	}
}

func TestSingleMode(t *testing.T) {
	s := mustParse(t, single(nil))
	expect(t, Evaluate(s, objs(cr(t, "ns", "c", "Succeeded=True/AllPassed"))), Success, "ns/c: Succeeded=True")
	expect(t, Evaluate(s, objs(cr(t, "ns", "c", "Failed=True/CategoryFailed"))), Failure, "ns/c: Failed=True (CategoryFailed)")
	expect(t, Evaluate(s, objs(cr(t, "ns", "c", "Succeeded=False/InProgress"))), Pending, "Succeeded=False (InProgress)")
	expect(t, Evaluate(s, objs(cr(t, "ns", "c"))), Pending, "Succeeded missing")
	// No status at all.
	expect(t, Evaluate(s, objs(obj(t, `{"metadata":{"namespace":"ns","name":"c"}}`))), Pending, "Succeeded missing")
	// Other objects in the snapshot are ignored: same name elsewhere, other names here.
	expect(t, Evaluate(s, objs(cr(t, "other", "c", "Succeeded=True"), cr(t, "ns", "d", "Succeeded=True"))), Pending, "ns/c not found")
}

func TestSingleModeTie(t *testing.T) {
	s := mustParse(t, single(nil))
	// Both predicates hold: failure wins.
	expect(t, Evaluate(s, objs(cr(t, "ns", "c", "Succeeded=True", "Failed=True"))), Failure, "Failed=True")
}

func TestAbsent(t *testing.T) {
	for _, tc := range []struct {
		absent Str
		want   Verdict
		reason string
	}{
		{Str{}, Pending, "ns/c not found"},
		{S("pending"), Pending, "ns/c not found"},
		{S("success"), Success, "not found (absent = success)"},
		{S("failure"), Failure, "not found (absent = failure)"},
	} {
		s := mustParse(t, single(func(r *Raw) { r.Absent = tc.absent }))
		expect(t, Evaluate(s, nil), tc.want, tc.reason)
	}
}

func TestConditionMatching(t *testing.T) {
	// reason is matched only when given; status is exact and case-sensitive.
	s := mustParse(t, single(func(r *Raw) {
		r.SuccessConditions = Cs(CR("Succeeded", "True", "AllPassed"))
		r.FailureConditions = Conds{}
	}))
	expect(t, Evaluate(s, objs(cr(t, "ns", "c", "Succeeded=True/AllPassed"))), Success, "")
	expect(t, Evaluate(s, objs(cr(t, "ns", "c", "Succeeded=True/Partial"))), Pending, "Succeeded=True (Partial)")
	expect(t, Evaluate(s, objs(cr(t, "ns", "c", "Succeeded=true/AllPassed"))), Pending, "")
	// Multiple success conditions: all must hold.
	s = mustParse(t, single(func(r *Raw) { r.SuccessConditions = Cs(C("A", "True"), C("B", "True")) }))
	expect(t, Evaluate(s, objs(cr(t, "ns", "c", "A=True"))), Pending, "B missing")
	expect(t, Evaluate(s, objs(cr(t, "ns", "c", "A=True", "B=True"))), Success, "")
	// Multiple failure conditions: any one fails.
	s = mustParse(t, single(func(r *Raw) { r.FailureConditions = Cs(C("X", "True"), C("Y", "True")) }))
	expect(t, Evaluate(s, objs(cr(t, "ns", "c", "Y=True"))), Failure, "Y=True")
}

func TestExpressions(t *testing.T) {
	s := mustParse(t, single(func(r *Raw) {
		r.SuccessConditions = Conds{}
		r.FailureConditions = Conds{}
		r.Expression = S("object.status.phase == 'Running'")
		r.FailureExpression = S("object.status.phase == 'Failed'")
	}))
	phase := func(p string) map[string]any {
		return obj(t, `{"metadata":{"namespace":"ns","name":"c"},"status":{"phase":"`+p+`"}}`)
	}
	expect(t, Evaluate(s, objs(phase("Running"))), Success, "expression true")
	expect(t, Evaluate(s, objs(phase("Failed"))), Failure, "failure_expression true")
	expect(t, Evaluate(s, objs(phase("Pending"))), Pending, "expression false")
	// expression is ANDed with success_conditions.
	s = mustParse(t, single(func(r *Raw) { r.Expression = S("object.metadata.name == 'x'") }))
	expect(t, Evaluate(s, objs(cr(t, "ns", "c", "Succeeded=True"))), Pending, "expression false")
}

// A CEL runtime error never produces success or failure by itself: it pins
// pending and names the error. A failure proven elsewhere still wins.
func TestCELRuntimeErrors(t *testing.T) {
	// Single mode: expression errors (no such key).
	s := mustParse(t, single(func(r *Raw) { r.Expression = S("object.status.phase == 'Done'") }))
	expect(t, Evaluate(s, objs(cr(t, "ns", "c", "Succeeded=True"))), Pending, "CEL error: expression: no such key: phase")
	// ...but a failure condition proven without it still fails.
	expect(t, Evaluate(s, objs(cr(t, "ns", "c", "Succeeded=True", "Failed=True"))), Failure, "Failed=True")
	// A failing success condition short-circuits: no error, plain pending.
	out := Evaluate(s, objs(cr(t, "ns", "c", "Succeeded=False")))
	expect(t, out, Pending, "Succeeded=False")
	if len(out.EvalErrors) != 0 {
		t.Errorf("unexpected eval errors %v", out.EvalErrors)
	}

	// failure_expression error pins pending even though success holds.
	s = mustParse(t, single(func(r *Raw) { r.FailureExpression = S("object.status.phase == 'Failed'") }))
	expect(t, Evaluate(s, objs(cr(t, "ns", "c", "Succeeded=True"))), Pending, "failure_expression: no such key")

	// Set mode: a filter error on one object cannot let a drain pass vacuously.
	drain := mustParse(t, Raw{APIVersion: S("v1"), Kind: S("Service"), Timeout: S("10m"),
		Filter: S("object.spec.type == 'LoadBalancer'"), MinMatching: N(0), MaxMatching: N(0)})
	noSpec := obj(t, `{"metadata":{"namespace":"a","name":"weird"}}`)
	clusterIP := obj(t, `{"metadata":{"namespace":"a","name":"svc"},"spec":{"type":"ClusterIP"}}`)
	out = Evaluate(drain, objs(clusterIP, noSpec))
	expect(t, out, Pending, "CEL error: a/weird: filter: no such key: spec")
	if len(out.EvalErrors) != 1 {
		t.Errorf("eval errors = %v", out.EvalErrors)
	}

	// Set mode: an error on one object loses to a failure on another.
	set := mustParse(t, Raw{APIVersion: S("v1"), Kind: S("Pod"), Timeout: S("10m"),
		Expression: S("object.status.ready"), FailureConditions: Cs(C("Failed", "True"))})
	expect(t, Evaluate(set, objs(cr(t, "ns", "a"), cr(t, "ns", "b", "Failed=True"))), Failure, "ns/b: Failed=True")
	// set_expression errors pin pending too.
	se := mustParse(t, Raw{APIVersion: S("v1"), Kind: S("Pod"), Timeout: S("10m"), SetExpression: S("objects[5].metadata.name == 'x'")})
	expect(t, Evaluate(se, objs(cr(t, "ns", "a"))), Pending, "CEL error: set_expression")
}

func setSpec(t *testing.T, mod func(r *Raw)) *Spec {
	r := Raw{APIVersion: S("v1"), Kind: S("Node"), Timeout: S("30m"), SuccessConditions: Cs(C("Ready", "True"))}
	mod(&r)
	return mustParse(t, r)
}

func nodes(t *testing.T, ready ...bool) []map[string]any {
	var out []map[string]any
	for i, r := range ready {
		st := "False/KubeletNotReady"
		if r {
			st = "True/KubeletReady"
		}
		out = append(out, cr(t, "", string(rune('a'+i)), "Ready="+st))
	}
	return out
}

func TestSetCounts(t *testing.T) {
	s := setSpec(t, func(r *Raw) { r.MinMatching = N(2); r.MaxMatching = N(3) })
	expect(t, Evaluate(s, nodes(t, true)), Pending, "1/1 pass; need 2 to 3")
	expect(t, Evaluate(s, nodes(t, true, false)), Pending, "1/2 pass; need 2 to 3; b: Ready=False (KubeletNotReady)")
	expect(t, Evaluate(s, nodes(t, true, true)), Success, "2/2 pass")
	expect(t, Evaluate(s, nodes(t, true, true, true, false)), Success, "3/4 pass")
	expect(t, Evaluate(s, nodes(t, true, true, true, true)), Pending, "4 pass; need 2 to 3: a; b; c; d")

	// Defaults: at least one, unbounded.
	s = setSpec(t, func(r *Raw) {})
	expect(t, Evaluate(s, nil), Pending, "0/0 pass; need at least 1")
	expect(t, Evaluate(s, nodes(t, false, true)), Success, "1/2 pass")
	expect(t, Evaluate(s, nodes(t, true, true, true, true, true, true)), Success, "")

	// Exactly N.
	s = setSpec(t, func(r *Raw) { r.MinMatching = N(2); r.MaxMatching = N(2) })
	expect(t, Evaluate(s, nodes(t, true)), Pending, "need exactly 2")
}

func TestRequireAll(t *testing.T) {
	s := setSpec(t, func(r *Raw) { r.MinMatching = N(2); r.RequireAll = B(true) })
	expect(t, Evaluate(s, nodes(t, true, true, false)), Pending, "2/3 pass; require_all: c: Ready=False (KubeletNotReady)")
	expect(t, Evaluate(s, nodes(t, true, true, true)), Success, "3/3 pass")
}

func TestFilterAndSetExpression(t *testing.T) {
	s := setSpec(t, func(r *Raw) {
		r.Filter = S("object.metadata.name != 'b'")
		r.RequireAll = B(true)
		r.SetExpression = S("objects.all(o, o.metadata.name in ['a', 'c'])")
	})
	// b is not ready but filtered out.
	out := Evaluate(s, nodes(t, true, false, true))
	expect(t, out, Success, "2/2 pass")
	if out.Matched != 2 || len(out.Objects) != 2 {
		t.Errorf("matched = %d, objects = %d", out.Matched, len(out.Objects))
	}
	// d is a stranger: set_expression false.
	expect(t, Evaluate(s, nodes(t, true, false, true, true)), Pending, "3/3 pass; set_expression false")
}

func TestSetFailureWinsTies(t *testing.T) {
	s := setSpec(t, func(r *Raw) { r.FailureConditions = Cs(C("Failed", "True")) })
	// One failing object among many passing fails the set.
	objs := nodes(t, true, true, true)
	objs = append(objs, cr(t, "", "z", "Ready=True", "Failed=True/Boom"))
	expect(t, Evaluate(s, objs), Failure, "1 matched object(s) failed: z: Failed=True (Boom)")
}

func TestDrain(t *testing.T) {
	s := mustParse(t, Raw{APIVersion: S("v1"), Kind: S("Pod"), Namespace: S("ns"), Timeout: S("10m"),
		MinMatching: N(0), MaxMatching: N(0)})
	if !s.Drain() {
		t.Fatal("expected a drain")
	}
	expect(t, Evaluate(s, nil), Success, "no objects match")
	expect(t, Evaluate(s, objs(cr(t, "ns", "b"), cr(t, "ns", "a"))), Pending, "2 still match: ns/a; ns/b")

	// Capped listing.
	var many []map[string]any
	for i := 0; i < 13; i++ {
		many = append(many, cr(t, "ns", string(rune('a'+i))))
	}
	expect(t, Evaluate(s, many), Pending, "13 still match: ns/a; ns/b; ns/c; ns/d; ns/e; ns/f; ns/g; ns/h; ns/i; ns/j; and 3 more")

	// Filtered drain (use 5 shape): ClusterIP services do not count.
	lb := mustParse(t, Raw{APIVersion: S("v1"), Kind: S("Service"), Timeout: S("10m"),
		Filter: S("object.spec.type == 'LoadBalancer'"), MinMatching: N(0), MaxMatching: N(0)})
	svc := func(ns, name, typ string) map[string]any {
		return obj(t, `{"metadata":{"namespace":"`+ns+`","name":"`+name+`"},"spec":{"type":"`+typ+`"}}`)
	}
	expect(t, Evaluate(lb, objs(svc("default", "kubernetes", "ClusterIP"))), Success, "")
	expect(t, Evaluate(lb, objs(svc("default", "kubernetes", "ClusterIP"), svc("t", "web", "LoadBalancer"))), Pending, "1 still match: t/web")
}
