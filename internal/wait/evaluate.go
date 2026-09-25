package wait

import (
	"fmt"
	"sort"
	"strings"
)

// maxListed caps how many objects a reason names.
const maxListed = 10

// Ref names an object.
type Ref struct{ Namespace, Name string }

func (r Ref) String() string {
	if r.Namespace == "" {
		return r.Name
	}
	return r.Namespace + "/" + r.Name
}

// RefOf reads an object's namespace and name.
func RefOf(obj map[string]any) Ref {
	md, _ := obj["metadata"].(map[string]any)
	ns, _ := md["namespace"].(string)
	name, _ := md["name"].(string)
	return Ref{Namespace: ns, Name: name}
}

// Outcome is the result of evaluating a spec against one snapshot.
type Outcome struct {
	Verdict Verdict
	Reason  string
	// Objects are the observed objects, sorted: the matched set in set mode,
	// the named object (if present) in single mode.
	Objects []map[string]any
	// Matched and Passing count objects in set mode.
	Matched, Passing int
	// EvalErrors are CEL runtime errors; any of them pins the verdict at
	// pending unless a failure is proven elsewhere.
	EvalErrors []string
}

// judgement is one object's standing against the per-object predicates.
type judgement struct {
	ref        Ref
	obj        map[string]any
	fails      bool
	failWhy    string
	passes     bool
	notPassWhy string
	err        error
}

func (s *Spec) judge(obj map[string]any) judgement {
	j := judgement{ref: RefOf(obj), obj: obj}

	// Failure: any failure condition, or failure_expression.
	for _, c := range s.FailureConditions {
		if c.holds(obj) {
			j.fails, j.failWhy = true, c.observed(obj)
			break
		}
	}
	var ferr error
	if !j.fails && s.FailureExpression != nil {
		v, err := s.FailureExpression.evalObject(obj)
		switch {
		case err != nil:
			ferr = err
		case v:
			j.fails, j.failWhy = true, "failure_expression true"
		}
	}

	// Success: every success condition, and expression.
	var unmet []string
	for _, c := range s.SuccessConditions {
		if !c.holds(obj) {
			unmet = append(unmet, c.observed(obj))
		}
	}
	var perr error
	switch {
	case len(unmet) > 0:
		j.notPassWhy = strings.Join(unmet, ", ")
	case s.Expression != nil:
		v, err := s.Expression.evalObject(obj)
		switch {
		case err != nil:
			perr = err
		case v:
			j.passes = true
		default:
			j.notPassWhy = "expression false"
		}
	default:
		j.passes = true
	}

	// An evaluation error leaves the object unclassified unless a failure
	// was proven without it.
	if !j.fails {
		if ferr != nil {
			j.err = ferr
		} else if perr != nil {
			j.err = perr
		}
	}
	return j
}

// Evaluate judges one snapshot of objects. In set mode objs is the
// server-side selection; in single-object mode the named object is picked
// out of objs by name (and namespace), so the caller may pass a superset.
func Evaluate(s *Spec, objs []map[string]any) Outcome {
	sorted := append([]map[string]any(nil), objs...)
	sort.SliceStable(sorted, func(a, b int) bool {
		ra, rb := RefOf(sorted[a]), RefOf(sorted[b])
		if ra.Namespace != rb.Namespace {
			return ra.Namespace < rb.Namespace
		}
		return ra.Name < rb.Name
	})
	if s.Single() {
		return s.evaluateSingle(sorted)
	}
	return s.evaluateSet(sorted)
}

func (s *Spec) evaluateSingle(objs []map[string]any) Outcome {
	var obj map[string]any
	for _, o := range objs {
		r := RefOf(o)
		if r.Name == s.Name && (s.Namespace == "" || r.Namespace == s.Namespace) {
			obj = o
			break
		}
	}
	ref := Ref{Namespace: s.Namespace, Name: s.Name}
	if obj == nil {
		out := Outcome{Verdict: s.Absent, Reason: ref.String() + " not found"}
		if s.Absent != Pending {
			out.Reason += fmt.Sprintf(" (absent = %s)", s.Absent)
		}
		return out
	}
	j := s.judge(obj)
	out := Outcome{Objects: []map[string]any{obj}, Matched: 1}
	switch {
	case j.fails:
		out.Verdict, out.Reason = Failure, ref.String()+": "+j.failWhy
	case j.err != nil:
		out.Verdict, out.Reason = Pending, ref.String()+": CEL error: "+j.err.Error()
		out.EvalErrors = []string{ref.String() + ": " + j.err.Error()}
	case j.passes:
		out.Verdict, out.Reason, out.Passing = Success, ref.String()+": "+s.passWhy(), 1
	default:
		out.Verdict, out.Reason = Pending, ref.String()+": "+j.notPassWhy
	}
	return out
}

func (s *Spec) passWhy() string {
	var parts []string
	for _, c := range s.SuccessConditions {
		parts = append(parts, c.String())
	}
	if s.Expression != nil {
		parts = append(parts, "expression true")
	}
	if len(parts) == 0 {
		return "present"
	}
	return strings.Join(parts, ", ")
}

func (s *Spec) evaluateSet(objs []map[string]any) Outcome {
	var out Outcome
	var matched []judgement
	for _, o := range objs {
		if s.Filter != nil {
			ok, err := s.Filter.evalObject(o)
			if err != nil {
				out.EvalErrors = append(out.EvalErrors, RefOf(o).String()+": "+err.Error())
				continue
			}
			if !ok {
				continue
			}
		}
		j := s.judge(o)
		if j.err != nil {
			out.EvalErrors = append(out.EvalErrors, j.ref.String()+": "+j.err.Error())
		}
		matched = append(matched, j)
		out.Objects = append(out.Objects, o)
	}
	out.Matched = len(matched)

	var failing, passing, notPassing []judgement
	for _, j := range matched {
		if j.fails {
			failing = append(failing, j)
		}
		if j.passes {
			passing = append(passing, j)
		} else if j.err == nil {
			notPassing = append(notPassing, j)
		}
	}
	out.Passing = len(passing)

	// Failure wins ties, and a proven failure beats an evaluation error.
	if len(failing) > 0 {
		out.Verdict = Failure
		out.Reason = fmt.Sprintf("%d matched object(s) failed: %s", len(failing), list(failing, func(j judgement) string {
			return j.ref.String() + ": " + j.failWhy
		}))
		return out
	}
	out.Verdict = Pending
	if len(out.EvalErrors) > 0 {
		out.Reason = "CEL error: " + capped(out.EvalErrors)
		return out
	}

	n := int64(len(passing))
	switch {
	case s.Drain() && n > 0:
		out.Reason = fmt.Sprintf("%d still match: %s", n, list(passing, refOnly))
		return out
	case s.MaxMatching >= 0 && n > s.MaxMatching:
		out.Reason = fmt.Sprintf("%d pass; %s: %s", n, s.need(), list(passing, refOnly))
		return out
	case n < s.MinMatching:
		out.Reason = fmt.Sprintf("%d/%d pass; %s", n, len(matched), s.need())
		if len(notPassing) > 0 {
			out.Reason += "; " + list(notPassing, whyNot)
		}
		return out
	case s.RequireAll && len(passing) != len(matched):
		out.Reason = fmt.Sprintf("%d/%d pass; require_all: %s", n, len(matched), list(notPassing, whyNot))
		return out
	}
	if s.SetExpression != nil {
		ok, err := s.SetExpression.evalObjects(out.Objects)
		if err != nil {
			out.EvalErrors = append(out.EvalErrors, err.Error())
			out.Reason = "CEL error: " + err.Error()
			return out
		}
		if !ok {
			out.Reason = fmt.Sprintf("%d/%d pass; set_expression false", n, len(matched))
			return out
		}
	}
	out.Verdict = Success
	if s.Drain() {
		out.Reason = "no objects match"
	} else {
		out.Reason = fmt.Sprintf("%d/%d pass; %s", n, len(matched), s.need())
	}
	return out
}

func (s *Spec) need() string {
	switch {
	case s.MaxMatching < 0:
		return fmt.Sprintf("need at least %d", s.MinMatching)
	case s.MinMatching == s.MaxMatching:
		return fmt.Sprintf("need exactly %d", s.MinMatching)
	default:
		return fmt.Sprintf("need %d to %d", s.MinMatching, s.MaxMatching)
	}
}

func refOnly(j judgement) string { return j.ref.String() }
func whyNot(j judgement) string  { return j.ref.String() + ": " + j.notPassWhy }

func list(js []judgement, f func(judgement) string) string {
	items := make([]string, len(js))
	for i, j := range js {
		items[i] = f(j)
	}
	return capped(items)
}

func capped(items []string) string {
	if len(items) <= maxListed {
		return strings.Join(items, "; ")
	}
	return strings.Join(items[:maxListed], "; ") + fmt.Sprintf("; and %d more", len(items)-maxListed)
}
