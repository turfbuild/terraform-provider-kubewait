package wait

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// base is a minimal valid set-mode config.
func base() Raw {
	return Raw{APIVersion: S("v1"), Kind: S("Pod"), Namespace: S("ns"), Timeout: S("10m")}
}

func TestParseDefaults(t *testing.T) {
	s := mustParse(t, base())
	if s.GVK.Group != "" || s.GVK.Version != "v1" || s.GVK.Kind != "Pod" {
		t.Errorf("GVK = %v", s.GVK)
	}
	if s.Single() {
		t.Error("expected set mode")
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"min_matching", s.MinMatching, int64(1)},
		{"max_matching", s.MaxMatching, int64(-1)},
		{"absent", s.Absent, Pending},
		{"settle", s.Settle, time.Duration(0)},
		{"poll_interval", s.PollInterval, 10 * time.Second},
		{"progress_interval", s.ProgressInterval, 60 * time.Second},
		{"watch", s.Watch, true},
		{"timeout", s.Timeout, 10 * time.Minute},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

	g := base()
	g.APIVersion = S("nvcre.nvidia.com/v1alpha1")
	g.Kind = S("Certification")
	if s := mustParse(t, g); s.GVK.Group != "nvcre.nvidia.com" || s.GVK.Version != "v1alpha1" {
		t.Errorf("GVK = %v", s.GVK)
	}
}

func TestParseValidation(t *testing.T) {
	tests := []struct {
		name     string
		mod      func(r *Raw)
		errs     []string
		warns    []string
		contains string // substring of the first error's detail
	}{
		// The five rejections the spec lists.
		{name: "rule1 name with label_selector", mod: func(r *Raw) { r.Name = S("a"); r.LabelSelector = S("x=y") },
			errs: []string{"label_selector"}, contains: "name selects a single object"},
		{name: "rule1 name with field_selector", mod: func(r *Raw) { r.Name = S("a"); r.FieldSelector = S("spec.nodeName=n") },
			errs: []string{"field_selector"}},
		{name: "rule1 name with both selectors", mod: func(r *Raw) {
			r.Name = S("a")
			r.LabelSelector = S("x=y")
			r.FieldSelector = S("spec.nodeName=n")
		}, errs: []string{"label_selector", "field_selector"}},
		{name: "rule2 min > max", mod: func(r *Raw) { r.MinMatching = N(3); r.MaxMatching = N(2) },
			errs: []string{"min_matching"}, contains: "min_matching (3) is greater than max_matching (2)"},
		{name: "rule2 default min > max 0", mod: func(r *Raw) { r.MaxMatching = N(0) },
			errs: []string{"min_matching"}, contains: "A drain needs min_matching = 0"},
		{name: "rule3 settle == timeout", mod: func(r *Raw) { r.Settle = S("10m") },
			errs: []string{"settle"}, contains: "shorter than timeout"},
		{name: "rule3 settle > timeout", mod: func(r *Raw) { r.Settle = S("20m") }, errs: []string{"settle"}},
		{name: "rule4 bad CEL syntax in filter", mod: func(r *Raw) { r.Filter = S("object.spec.type ==") },
			errs: []string{"filter"}, contains: "Syntax error"},
		{name: "rule4 undeclared variable", mod: func(r *Raw) { r.Expression = S("objects.size() > 0") },
			errs: []string{"expression"}, contains: "undeclared reference to 'objects'"},
		{name: "rule4 object in set_expression", mod: func(r *Raw) { r.SetExpression = S("object.metadata.name == 'a'") },
			errs: []string{"set_expression"}},
		{name: "rule4 non-bool result", mod: func(r *Raw) { r.FailureExpression = S("'failed'") },
			errs: []string{"failure_expression"}, contains: "must evaluate to bool"},
		{name: "rule5 drain with success_conditions", mod: func(r *Raw) {
			r.MinMatching, r.MaxMatching = N(0), N(0)
			r.SuccessConditions = Cs(C("Ready", "True"))
		}, errs: []string{"success_conditions"}, contains: "Use filter to narrow the drained set"},
		{name: "rule5 drain with expression", mod: func(r *Raw) {
			r.MinMatching, r.MaxMatching = N(0), N(0)
			r.Expression = S("true")
		}, errs: []string{"expression"}},

		// Malformed input.
		{name: "bad api_version", mod: func(r *Raw) { r.APIVersion = S("a/b/c") }, errs: []string{"api_version"}},
		{name: "empty api_version", mod: func(r *Raw) { r.APIVersion = S("") }, errs: []string{"api_version"}},
		{name: "empty kind", mod: func(r *Raw) { r.Kind = S("") }, errs: []string{"kind"}},
		{name: "empty name", mod: func(r *Raw) { r.Name = S("") }, errs: []string{"name"}},
		{name: "empty namespace", mod: func(r *Raw) { r.Namespace = S("") }, errs: []string{"namespace"}},
		{name: "bad label_selector", mod: func(r *Raw) { r.LabelSelector = S("a in (") }, errs: []string{"label_selector"}},
		{name: "bad field_selector", mod: func(r *Raw) { r.FieldSelector = S("a") }, errs: []string{"field_selector"}},
		{name: "bad timeout", mod: func(r *Raw) { r.Timeout = S("10 minutes") }, errs: []string{"timeout"}},
		{name: "zero timeout", mod: func(r *Raw) { r.Timeout = S("0s") }, errs: []string{"timeout"}},
		{name: "negative settle", mod: func(r *Raw) { r.Settle = S("-1s") }, errs: []string{"settle"}},
		{name: "zero poll_interval", mod: func(r *Raw) { r.PollInterval = S("0s") }, errs: []string{"poll_interval"}},
		{name: "bad progress_interval", mod: func(r *Raw) { r.ProgressInterval = S("often") }, errs: []string{"progress_interval"}},
		{name: "bad absent", mod: func(r *Raw) { r.Name = S("a"); r.Absent = S("fail") }, errs: []string{"absent"}},
		{name: "fractional min", mod: func(r *Raw) { r.MinMatching = NF(1.5) }, errs: []string{"min_matching"}},
		{name: "negative max", mod: func(r *Raw) { r.MaxMatching = N(-1) }, errs: []string{"max_matching"}},
		{name: "empty condition type", mod: func(r *Raw) { r.SuccessConditions = Cs(C("", "True")) },
			errs: []string{"success_conditions[0].type"}},
		{name: "empty condition status", mod: func(r *Raw) { r.FailureConditions = Cs(C("Ready", "True"), C("Failed", "")) },
			errs: []string{"failure_conditions[1].status"}},
		{name: "bad progress field", mod: func(r *Raw) { r.ProgressFields = Ss("status.conditions", "a..b") },
			errs: []string{"progress_fields[1]"}},

		// Warnings, not errors.
		{name: "set-only attributes in single mode", mod: func(r *Raw) {
			r.Name = S("a")
			r.Filter = S("true")
			r.SetExpression = S("true")
			r.MinMatching = N(1)
			r.MaxMatching = N(1)
			r.RequireAll = B(true)
		}, warns: []string{"filter", "set_expression", "min_matching", "max_matching", "require_all"}},
		{name: "absent in set mode", mod: func(r *Raw) { r.Absent = S("failure") }, warns: []string{"absent"}},

		// Accepted.
		{name: "drain with filter and failure predicates", mod: func(r *Raw) {
			r.MinMatching, r.MaxMatching = N(0), N(0)
			r.Filter = S("object.spec.type == 'LoadBalancer'")
			r.FailureConditions = Cs(C("Failed", "True"))
			r.FailureExpression = S("has(object.status.phase) && object.status.phase == 'Failed'")
		}},
		{name: "dyn result accepted", mod: func(r *Raw) { r.Expression = S("object.spec.ready") }},
		{name: "condition with reason", mod: func(r *Raw) { r.SuccessConditions = Cs(CR("Ready", "True", "KubeletReady")) }},
		{name: "bracketed progress field", mod: func(r *Raw) {
			r.ProgressFields = Ss(`metadata.labels["app.kubernetes.io/name"]`, "spec.containers[0].image")
		}},
		{name: "set_expression uses ext libs", mod: func(r *Raw) {
			r.SetExpression = S("objects.map(o, o.metadata.name).sort() == ['a', 'b'] && sets.contains(['a'], ['a'])")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := base()
			tt.mod(&raw)
			s, probs := Parse(raw)
			if got := errorPaths(probs); !reflect.DeepEqual(got, tt.errs) {
				t.Fatalf("errors at %v, want %v\n%+v", got, tt.errs, probs)
			}
			if got := warningPaths(probs); !reflect.DeepEqual(got, tt.warns) {
				t.Errorf("warnings at %v, want %v", got, tt.warns)
			}
			if tt.contains != "" && !strings.Contains(probs[0].Detail, tt.contains) {
				t.Errorf("detail %q does not contain %q", probs[0].Detail, tt.contains)
			}
			if (s == nil) != (len(tt.errs) > 0) {
				t.Errorf("spec nil = %v with %d errors", s == nil, len(tt.errs))
			}
		})
	}
}

// Under `terraform validate`, variables and data sources are unknown. Checks
// that involve an unknown value are skipped; the rest still run; no spec.
func TestParseUnknowns(t *testing.T) {
	raw := base()
	raw.Name = unknownStr
	raw.LabelSelector = S("x=y") // rule 1 would need name
	raw.MinMatching = Num{Unknown: true}
	raw.MaxMatching = N(0) // rule 2 would need min
	raw.Settle = unknownStr
	raw.Expression = unknownStr
	raw.SuccessConditions = Conds{Unknown: true}
	raw.Filter = S("object.spec.type ==") // still checked
	s, probs := Parse(raw)
	if s != nil {
		t.Error("expected nil spec with unknowns")
	}
	if got := errorPaths(probs); !reflect.DeepEqual(got, []string{"filter"}) {
		t.Errorf("errors at %v, want [filter]", got)
	}
	if len(warningPaths(probs)) != 0 {
		t.Errorf("unexpected warnings %v", warningPaths(probs))
	}

	// An unknown element field is skipped too.
	raw = base()
	raw.SuccessConditions = Cs(RawCondition{Type: unknownStr, Status: S("True")})
	if s, probs := Parse(raw); s != nil || probs.HasError() {
		t.Errorf("spec=%v problems=%v", s, probs)
	}
}

func TestParseFieldPath(t *testing.T) {
	o := obj(t, `{"metadata":{"name":"n","labels":{"app.kubernetes.io/name":"x"}},
		"spec":{"containers":[{"image":"a"},{"image":"b"}]}}`)
	for path, want := range map[string]any{
		"metadata.name": "n",
		`metadata.labels["app.kubernetes.io/name"]`: "x",
		"spec.containers[1].image":                  "b",
	} {
		fp, err := ParseFieldPath(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if got, ok := fp.Lookup(o); !ok || got != want {
			t.Errorf("%s = %v, %v; want %v", path, got, ok, want)
		}
	}
	for _, path := range []string{"spec.missing", "spec.containers[5].image", "metadata.name.x"} {
		fp, _ := ParseFieldPath(path)
		if _, ok := fp.Lookup(o); ok {
			t.Errorf("%s: expected missing", path)
		}
	}
	for _, bad := range []string{"", ".a", "a.", "a..b", "a[", `a["x]`, "a[-1]", "a[x]", "a[0]b"} {
		if _, err := ParseFieldPath(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}
