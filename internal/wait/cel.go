package wait

import (
	"fmt"
	"strings"
	"sync"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/ext"
)

// costLimit bounds one evaluation. Kubernetes uses a per-expression budget
// of the same order for CRD validation rules.
const costLimit = 10_000_000

var (
	envOnce               sync.Once
	objectEnv, objectsEnv *cel.Env
	envErr                error
)

func envs() (*cel.Env, *cel.Env, error) {
	envOnce.Do(func() {
		libs := []cel.EnvOption{ext.Strings(), ext.Lists(), ext.Sets(), ext.Encoders()}
		objectEnv, envErr = cel.NewEnv(append(libs, cel.Variable("object", cel.MapType(cel.StringType, cel.DynType)))...)
		if envErr != nil {
			return
		}
		objectsEnv, envErr = cel.NewEnv(append(libs, cel.Variable("objects", cel.ListType(cel.DynType)))...)
	})
	return objectEnv, objectsEnv, envErr
}

// Program is a compiled CEL predicate.
type Program struct {
	Attr   string
	Source string
	prg    cel.Program
}

// Compile compiles a predicate over `object` (a single object as a map), or,
// when objects is true, over `objects` (the matched set as a list). An
// undeclared variable is a compile error, so `objects` in a per-object
// expression is rejected. The result type must be bool (or dyn).
func Compile(attr, src string, objects bool) (*Program, error) {
	objEnv, objsEnv, err := envs()
	if err != nil {
		return nil, err
	}
	env := objEnv
	if objects {
		env = objsEnv
	}
	if strings.TrimSpace(src) == "" {
		return nil, fmt.Errorf("expression is empty")
	}
	ast, iss := env.Compile(src)
	if iss.Err() != nil {
		return nil, iss.Err()
	}
	if ot := ast.OutputType(); !ot.IsExactType(cel.BoolType) && !ot.IsExactType(cel.DynType) {
		return nil, fmt.Errorf("expression must evaluate to bool, not %s", cel.FormatCELType(ot))
	}
	// Type-check for validation, but run the unchecked parse. The checker
	// binds the result type of an index on dyn to whatever consumes it, so
	// int(object.status.allocatable["nvidia.com/gpu"]) is checked as
	// int(int) and fails at runtime with "no such overload: int(string)"
	// (cel-go v0.29 through v0.32). Unchecked, every call dispatches on the
	// runtime type, which is all a map(string, dyn) object can offer anyway.
	parsed, iss := env.Parse(src)
	if iss.Err() != nil {
		return nil, iss.Err()
	}
	prg, err := env.Program(parsed, cel.CostLimit(costLimit))
	if err != nil {
		return nil, err
	}
	return &Program{Attr: attr, Source: src, prg: prg}, nil
}

// evalObject evaluates a per-object predicate.
func (p *Program) evalObject(obj map[string]any) (bool, error) {
	return p.eval(map[string]any{"object": obj})
}

// evalObjects evaluates a set predicate.
func (p *Program) evalObjects(objs []map[string]any) (bool, error) {
	list := make([]any, len(objs))
	for i, o := range objs {
		list[i] = o
	}
	return p.eval(map[string]any{"objects": list})
}

func (p *Program) eval(vars map[string]any) (bool, error) {
	out, _, err := p.prg.Eval(vars)
	if err != nil {
		return false, fmt.Errorf("%s: %w", p.Attr, err)
	}
	b, ok := out.(types.Bool)
	if !ok {
		return false, fmt.Errorf("%s: evaluated to %s, not bool", p.Attr, out.Type())
	}
	return bool(b), nil
}
