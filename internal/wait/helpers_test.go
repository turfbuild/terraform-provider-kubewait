package wait

import (
	"fmt"
	"math/big"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/json"
)

func S(v string) Str { return Str{V: v, Set: true} }

var unknownStr = Str{Unknown: true}

func N(n int64) Num    { return Num{V: new(big.Float).SetInt64(n), Set: true} }
func NF(f float64) Num { return Num{V: big.NewFloat(f), Set: true} }
func B(b bool) Bool    { return Bool{V: b, Set: true} }
func Ss(vs ...string) Strs {
	out := Strs{Set: true}
	for _, v := range vs {
		out.Items = append(out.Items, S(v))
	}
	return out
}

func C(typ, status string) RawCondition { return RawCondition{Type: S(typ), Status: S(status)} }
func CR(typ, status, reason string) RawCondition {
	return RawCondition{Type: S(typ), Status: S(status), Reason: S(reason)}
}
func Cs(cs ...RawCondition) Conds { return Conds{Items: cs, Set: true} }

func mustParse(t *testing.T, raw Raw) *Spec {
	t.Helper()
	s, probs := Parse(raw)
	for _, p := range probs {
		if p.Severity == Error {
			t.Fatalf("unexpected error at %v: %s: %s", p.Path, p.Summary, p.Detail)
		}
	}
	if s == nil {
		t.Fatalf("Parse returned nil spec without errors")
	}
	return s
}

// obj decodes a JSON object the way client-go's unstructured decoder does
// (integers as int64).
func obj(t *testing.T, js string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		t.Fatalf("bad fixture: %v\n%s", err, js)
	}
	return m
}

// cr is a namespaced object carrying the given conditions, each as
// "Type=Status" or "Type=Status/Reason".
func cr(t *testing.T, ns, name string, conds ...string) map[string]any {
	t.Helper()
	var cs []string
	for _, c := range conds {
		typ, rest, _ := strings.Cut(c, "=")
		status, reason, _ := strings.Cut(rest, "/")
		cs = append(cs, fmt.Sprintf(`{"type":%q,"status":%q,"reason":%q}`, typ, status, reason))
	}
	nsField := ""
	if ns != "" {
		nsField = fmt.Sprintf(`,"namespace":%q`, ns)
	}
	return obj(t, fmt.Sprintf(`{"metadata":{"name":%q%s},"status":{"conditions":[%s]}}`, name, nsField, strings.Join(cs, ",")))
}

func objs(os ...map[string]any) []map[string]any { return os }

func errorPaths(probs Problems) []string {
	var out []string
	for _, p := range probs {
		if p.Severity == Error {
			out = append(out, p.PathString())
		}
	}
	return out
}

func warningPaths(probs Problems) []string {
	var out []string
	for _, p := range probs {
		if p.Severity == Warning {
			out = append(out, p.PathString())
		}
	}
	return out
}
