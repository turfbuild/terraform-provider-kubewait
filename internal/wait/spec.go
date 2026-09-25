// Package wait is the pure core of kubewait_condition: parsing and validating
// a wait configuration, evaluating it against a snapshot of objects, and the
// settle clock. It performs no I/O; internal/watcher feeds it observations.
package wait

import (
	"fmt"
	"math/big"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Defaults for optional attributes. The action schema cannot carry defaults
// (actions have no computed attributes), so they live here and in the schema
// descriptions.
const (
	DefaultMinMatching      = 1
	DefaultPollInterval     = 10 * time.Second
	DefaultProgressInterval = 60 * time.Second
)

// Verdict is the state of a wait at one observation.
type Verdict int

const (
	Pending Verdict = iota
	Success
	Failure
)

func (v Verdict) String() string {
	switch v {
	case Success:
		return "success"
	case Failure:
		return "failure"
	default:
		return "pending"
	}
}

// Condition is one {type, status, reason?} entry.
type Condition struct {
	Type   string
	Status string
	// Reason is matched only when HasReason is set.
	Reason    string
	HasReason bool
}

func (c Condition) String() string {
	if c.HasReason {
		return fmt.Sprintf("%s=%s (%s)", c.Type, c.Status, c.Reason)
	}
	return c.Type + "=" + c.Status
}

// Spec is a parsed, validated wait configuration with defaults applied.
type Spec struct {
	GVK           schema.GroupVersionKind
	Namespace     string
	Name          string // non-empty: single-object mode
	LabelSelector string
	FieldSelector string

	Filter            *Program
	Expression        *Program
	FailureExpression *Program
	SetExpression     *Program

	SuccessConditions []Condition
	FailureConditions []Condition

	MinMatching int64
	MaxMatching int64 // -1: unbounded
	RequireAll  bool
	Absent      Verdict

	Timeout          time.Duration
	Settle           time.Duration
	PollInterval     time.Duration
	ProgressInterval time.Duration
	Watch            bool

	ProgressFields []FieldPath
}

// Single reports whether the spec is in single-object mode.
func (s *Spec) Single() bool { return s.Name != "" }

// Drain reports whether the spec waits for its matched set to empty.
func (s *Spec) Drain() bool { return s.MaxMatching == 0 && !s.hasSuccessPredicates() }

func (s *Spec) hasSuccessPredicates() bool {
	return len(s.SuccessConditions) > 0 || s.Expression != nil
}

// Str is a configuration string that may be null or not yet known.
type Str struct {
	V       string
	Set     bool // false: null
	Unknown bool
}

// Num is a configuration number that may be null or not yet known.
type Num struct {
	V       *big.Float
	Set     bool
	Unknown bool
}

// Bool is a configuration boolean that may be null or not yet known.
type Bool struct {
	V       bool
	Set     bool
	Unknown bool
}

// RawCondition is one element of success_conditions / failure_conditions.
type RawCondition struct {
	Type, Status, Reason Str
}

// Conds is a list of conditions that may be null or not yet known.
type Conds struct {
	Items   []RawCondition
	Set     bool
	Unknown bool
}

// Strs is a list of strings that may be null or not yet known.
type Strs struct {
	Items   []Str
	Set     bool
	Unknown bool
}

// Raw is the action configuration as Terraform delivers it: every value may
// be null, and at validate/plan time any of them may be unknown.
type Raw struct {
	APIVersion, Kind              Str
	Namespace, Name               Str
	LabelSelector, FieldSelector  Str
	Filter                        Str
	SuccessConditions             Conds
	FailureConditions             Conds
	Expression, FailureExpression Str
	SetExpression                 Str
	MinMatching, MaxMatching      Num
	RequireAll                    Bool
	Absent                        Str
	Timeout, Settle, PollInterval Str
	Watch                         Bool
	ProgressFields                Strs
	ProgressInterval              Str
}

// Severity of a Problem.
type Severity int

const (
	Error Severity = iota
	Warning
)

// Problem is a validation finding. Path is an attribute path: attribute
// names (string) and list indices (int).
type Problem struct {
	Severity Severity
	Path     []any
	Summary  string
	Detail   string
}

// PathString renders Path as success_conditions[0].type.
func (pr Problem) PathString() string {
	var b strings.Builder
	for _, e := range pr.Path {
		switch v := e.(type) {
		case int:
			fmt.Fprintf(&b, "[%d]", v)
		default:
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			fmt.Fprint(&b, v)
		}
	}
	return b.String()
}

// Problems is a list of validation findings.
type Problems []Problem

// HasError reports whether any problem is an error.
func (ps Problems) HasError() bool {
	for _, p := range ps {
		if p.Severity == Error {
			return true
		}
	}
	return false
}

func (ps *Problems) errorf(path []any, summary, format string, args ...any) {
	*ps = append(*ps, Problem{Severity: Error, Path: path, Summary: summary, Detail: fmt.Sprintf(format, args...)})
}

func (ps *Problems) warnf(path []any, summary, format string, args ...any) {
	*ps = append(*ps, Problem{Severity: Warning, Path: path, Summary: summary, Detail: fmt.Sprintf(format, args...)})
}

func p(elems ...any) []any { return elems }

// Parse validates raw and, when every value is known and valid, returns the
// Spec with defaults applied. Checks that involve an unknown value are
// skipped; the returned spec is nil if anything is unknown or invalid.
func Parse(raw Raw) (*Spec, Problems) {
	var probs Problems
	complete := true
	known := func(unknown bool) bool {
		if unknown {
			complete = false
		}
		return !unknown
	}

	s := &Spec{MinMatching: DefaultMinMatching, MaxMatching: -1, Absent: Pending,
		PollInterval: DefaultPollInterval, ProgressInterval: DefaultProgressInterval, Watch: true}

	// Kind.
	if known(raw.APIVersion.Unknown) && known(raw.Kind.Unknown) {
		gv, err := schema.ParseGroupVersion(raw.APIVersion.V)
		switch {
		case err != nil || raw.APIVersion.V == "" || gv.Version == "":
			probs.errorf(p("api_version"), "Invalid api_version",
				"api_version must be <group>/<version>, or <version> for the core group, such as \"v1\" or \"nvcre.nvidia.com/v1alpha1\"; got %q.", raw.APIVersion.V)
		case raw.Kind.V == "":
			probs.errorf(p("kind"), "Invalid kind", "kind must not be empty.")
		default:
			s.GVK = gv.WithKind(raw.Kind.V)
		}
	}

	// Mode and selection.
	nameKnown := known(raw.Name.Unknown)
	if nameKnown && raw.Name.Set {
		if raw.Name.V == "" {
			probs.errorf(p("name"), "Invalid name", "name must not be empty; omit it for set mode.")
		}
		s.Name = raw.Name.V
	}
	if known(raw.Namespace.Unknown) && raw.Namespace.Set {
		if raw.Namespace.V == "" {
			probs.errorf(p("namespace"), "Invalid namespace", "namespace must not be empty; omit it for cluster-scoped kinds or to select all namespaces.")
		}
		s.Namespace = raw.Namespace.V
	}
	if known(raw.LabelSelector.Unknown) && raw.LabelSelector.Set {
		if _, err := labels.Parse(raw.LabelSelector.V); err != nil {
			probs.errorf(p("label_selector"), "Invalid label_selector", "%s", err)
		}
		s.LabelSelector = raw.LabelSelector.V
	}
	if known(raw.FieldSelector.Unknown) && raw.FieldSelector.Set {
		if _, err := fields.ParseSelector(raw.FieldSelector.V); err != nil {
			probs.errorf(p("field_selector"), "Invalid field_selector", "%s", err)
		}
		s.FieldSelector = raw.FieldSelector.V
	}
	// Spec rule 1: name together with a selector.
	if nameKnown && raw.Name.Set {
		for _, sel := range []struct {
			attr string
			v    Str
		}{{"label_selector", raw.LabelSelector}, {"field_selector", raw.FieldSelector}} {
			if !sel.v.Unknown && sel.v.Set {
				probs.errorf(p(sel.attr), "Conflicting configuration",
					"name selects a single object, so %s cannot be set with it. Omit name for set mode, or drop %s.", sel.attr, sel.attr)
			}
		}
	}

	// Conditions.
	s.SuccessConditions = parseConds(&probs, "success_conditions", raw.SuccessConditions, &complete)
	s.FailureConditions = parseConds(&probs, "failure_conditions", raw.FailureConditions, &complete)

	// CEL (spec rule 4).
	compile := func(attr string, v Str, objects bool) *Program {
		if !known(v.Unknown) || !v.Set {
			return nil
		}
		prg, err := Compile(attr, v.V, objects)
		if err != nil {
			probs.errorf(p(attr), "Invalid CEL in "+attr, "%s", err)
			return nil
		}
		return prg
	}
	s.Filter = compile("filter", raw.Filter, false)
	s.Expression = compile("expression", raw.Expression, false)
	s.FailureExpression = compile("failure_expression", raw.FailureExpression, false)
	s.SetExpression = compile("set_expression", raw.SetExpression, true)

	// Counts.
	minKnown, maxKnown := known(raw.MinMatching.Unknown), known(raw.MaxMatching.Unknown)
	if minKnown && raw.MinMatching.Set {
		if n, ok := count(&probs, "min_matching", raw.MinMatching.V); ok {
			s.MinMatching = n
		} else {
			minKnown = false
		}
	}
	if maxKnown && raw.MaxMatching.Set {
		if n, ok := count(&probs, "max_matching", raw.MaxMatching.V); ok {
			s.MaxMatching = n
		} else {
			maxKnown = false
		}
	}
	// Spec rule 2: min_matching > max_matching (with the default min of 1).
	if minKnown && maxKnown && s.MaxMatching >= 0 && s.MinMatching > s.MaxMatching {
		detail := fmt.Sprintf("min_matching (%d) is greater than max_matching (%d), so the wait could never succeed.", s.MinMatching, s.MaxMatching)
		if !raw.MinMatching.Set {
			detail += fmt.Sprintf(" min_matching defaults to %d.", DefaultMinMatching)
		}
		if s.MaxMatching == 0 {
			detail += " A drain needs min_matching = 0."
		}
		probs.errorf(p("min_matching"), "Invalid min_matching", "%s", detail)
	}
	// Spec rule 5: a drain with success predicates.
	if maxKnown && raw.MaxMatching.Set && s.MaxMatching == 0 {
		for _, sp := range []struct {
			attr string
			set  bool
			unk  bool
		}{
			{"success_conditions", raw.SuccessConditions.Set && len(raw.SuccessConditions.Items) > 0, raw.SuccessConditions.Unknown},
			{"expression", raw.Expression.Set, raw.Expression.Unknown},
		} {
			if sp.set && !sp.unk {
				probs.errorf(p(sp.attr), "Drain with success predicates",
					"max_matching = 0 is a drain: it succeeds when no object matches. %s would narrow which objects count, so a drain could pass while matching objects remain. Use filter to narrow the drained set, and set min_matching = 0.", sp.attr)
			}
		}
	}

	if known(raw.RequireAll.Unknown) && raw.RequireAll.Set {
		s.RequireAll = raw.RequireAll.V
	}
	if known(raw.Watch.Unknown) && raw.Watch.Set {
		s.Watch = raw.Watch.V
	}
	if known(raw.Absent.Unknown) && raw.Absent.Set {
		switch raw.Absent.V {
		case "pending":
			s.Absent = Pending
		case "success":
			s.Absent = Success
		case "failure":
			s.Absent = Failure
		default:
			probs.errorf(p("absent"), "Invalid absent", "absent must be one of pending, success or failure; got %q.", raw.Absent.V)
		}
	}

	// Durations.
	timeoutOK, settleOK := false, true
	if known(raw.Timeout.Unknown) {
		if d, ok := duration(&probs, "timeout", raw.Timeout, true); ok {
			s.Timeout, timeoutOK = d, true
		}
	}
	if !known(raw.Settle.Unknown) {
		settleOK = false
	} else if raw.Settle.Set {
		d, ok := duration(&probs, "settle", raw.Settle, false)
		s.Settle, settleOK = d, ok
	}
	if known(raw.PollInterval.Unknown) && raw.PollInterval.Set {
		if d, ok := duration(&probs, "poll_interval", raw.PollInterval, true); ok {
			s.PollInterval = d
		}
	}
	if known(raw.ProgressInterval.Unknown) && raw.ProgressInterval.Set {
		if d, ok := duration(&probs, "progress_interval", raw.ProgressInterval, true); ok {
			s.ProgressInterval = d
		}
	}
	// Spec rule 3: settle >= timeout.
	if timeoutOK && settleOK && s.Settle >= s.Timeout {
		probs.errorf(p("settle"), "Invalid settle",
			"settle (%s) must be shorter than timeout (%s): a verdict that must hold for the whole budget can never settle before it runs out.", raw.Settle.V, raw.Timeout.V)
	}

	// Progress fields.
	if known(raw.ProgressFields.Unknown) && raw.ProgressFields.Set {
		for i, f := range raw.ProgressFields.Items {
			if !known(f.Unknown) {
				continue
			}
			fp, err := ParseFieldPath(f.V)
			if err != nil {
				probs.errorf(p("progress_fields", i), "Invalid progress_fields entry", "%s", err)
				continue
			}
			s.ProgressFields = append(s.ProgressFields, fp)
		}
	}

	// Attributes that do nothing in the chosen mode: warnings, not errors,
	// because the spec lists exactly what is rejected.
	if nameKnown {
		if raw.Name.Set {
			for _, a := range []struct {
				attr string
				set  bool
			}{
				{"filter", raw.Filter.Set},
				{"set_expression", raw.SetExpression.Set},
				{"min_matching", raw.MinMatching.Set},
				{"max_matching", raw.MaxMatching.Set},
				{"require_all", raw.RequireAll.Set},
			} {
				if a.set {
					probs.warnf(p(a.attr), "Attribute ignored in single-object mode",
						"%s applies to set mode only. With name set, the wait observes a single object and ignores %s.", a.attr, a.attr)
				}
			}
		} else if raw.Absent.Set {
			probs.warnf(p("absent"), "Attribute ignored in set mode",
				"absent applies to single-object mode only (name set). In set mode an empty set is judged by min_matching.")
		}
	}

	if !complete || probs.HasError() {
		return nil, probs
	}
	return s, probs
}

func parseConds(probs *Problems, attr string, raw Conds, complete *bool) []Condition {
	if raw.Unknown {
		*complete = false
		return nil
	}
	var out []Condition
	for i, rc := range raw.Items {
		c := Condition{}
		ok := true
		for _, f := range []struct {
			name string
			v    Str
			dst  *string
			req  bool
		}{{"type", rc.Type, &c.Type, true}, {"status", rc.Status, &c.Status, true}, {"reason", rc.Reason, &c.Reason, false}} {
			if f.v.Unknown {
				*complete = false
				ok = false
				continue
			}
			if !f.v.Set {
				continue
			}
			if f.v.V == "" && f.req {
				probs.errorf(p(attr, i, f.name), "Invalid condition", "%s must not be empty.", f.name)
				ok = false
			}
			*f.dst = f.v.V
		}
		c.HasReason = rc.Reason.Set && !rc.Reason.Unknown
		if ok {
			out = append(out, c)
		}
	}
	return out
}

func count(probs *Problems, attr string, v *big.Float) (int64, bool) {
	if v == nil {
		probs.errorf(p(attr), "Invalid "+attr, "%s must be a number.", attr)
		return 0, false
	}
	n, acc := v.Int64()
	if !v.IsInt() || v.Sign() < 0 || acc != big.Exact {
		probs.errorf(p(attr), "Invalid "+attr, "%s must be a whole number of objects, zero or more; got %s.", attr, v.Text('g', -1))
		return 0, false
	}
	return n, true
}

func duration(probs *Problems, attr string, v Str, positive bool) (time.Duration, bool) {
	d, err := time.ParseDuration(strings.TrimSpace(v.V))
	if err != nil {
		probs.errorf(p(attr), "Invalid "+attr, "%s must be a duration such as \"30s\", \"10m\" or \"1h30m\"; got %q.", attr, v.V)
		return 0, false
	}
	if positive && d <= 0 {
		probs.errorf(p(attr), "Invalid "+attr, "%s must be greater than zero; got %q.", attr, v.V)
		return 0, false
	}
	if d < 0 {
		probs.errorf(p(attr), "Invalid "+attr, "%s must not be negative; got %q.", attr, v.V)
		return 0, false
	}
	return d, true
}
