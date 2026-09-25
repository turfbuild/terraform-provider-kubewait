package provider

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func noEnv(t *testing.T) {
	t.Helper()
	old := getenv
	getenv = func(string) string { return "" }
	t.Cleanup(func() { getenv = old })
	// client-go reads this one itself, for the in-cluster fallback.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
}

func objectValue(t *testing.T, typ tftypes.Object, vals map[string]tftypes.Value) *tfprotov6.DynamicValue {
	t.Helper()
	all := map[string]tftypes.Value{}
	for name, at := range typ.AttributeTypes {
		if v, ok := vals[name]; ok {
			all[name] = v
		} else {
			all[name] = tftypes.NewValue(at, nil)
		}
	}
	dv, err := tfprotov6.NewDynamicValue(typ, tftypes.NewValue(typ, all))
	if err != nil {
		t.Fatal(err)
	}
	return &dv
}

func actionType(t *testing.T) tftypes.Object {
	return actionSchema(t).ValueType().(tftypes.Object)
}

func s(v string) tftypes.Value { return tftypes.NewValue(tftypes.String, v) }
func n(v int64) tftypes.Value  { return tftypes.NewValue(tftypes.Number, new(big.Float).SetInt64(v)) }

var unknownString = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)

func condList(t *testing.T, typ tftypes.Object, attr string, pairs ...string) tftypes.Value {
	lt := typ.AttributeTypes[attr].(tftypes.List)
	ot := lt.ElementType.(tftypes.Object)
	var elems []tftypes.Value
	for i := 0; i+1 < len(pairs); i += 2 {
		elems = append(elems, tftypes.NewValue(ot, map[string]tftypes.Value{
			"type": s(pairs[i]), "status": s(pairs[i+1]), "reason": tftypes.NewValue(tftypes.String, nil),
		}))
	}
	return tftypes.NewValue(lt, elems)
}

func diagText(ds []*tfprotov6.Diagnostic) string {
	var b strings.Builder
	for _, d := range ds {
		b.WriteString(string(rune('0'+d.Severity)) + " " + d.Summary + ": " + d.Detail)
		if d.Attribute != nil {
			b.WriteString(" @" + d.Attribute.String())
		}
		b.WriteString("\n")
	}
	return b.String()
}

func validate(t *testing.T, vals map[string]tftypes.Value) []*tfprotov6.Diagnostic {
	t.Helper()
	resp, err := protoServer(t).ValidateActionConfig(context.Background(), &tfprotov6.ValidateActionConfigRequest{
		ActionType: "kubewait_condition", Config: objectValue(t, actionType(t), vals)})
	if err != nil {
		t.Fatal(err)
	}
	return resp.Diagnostics
}

func plan(t *testing.T, vals map[string]tftypes.Value) []*tfprotov6.Diagnostic {
	t.Helper()
	resp, err := protoServer(t).PlanAction(context.Background(), &tfprotov6.PlanActionRequest{
		ActionType: "kubewait_condition", Config: objectValue(t, actionType(t), vals),
		ClientCapabilities: &tfprotov6.PlanActionClientCapabilities{DeferralAllowed: true}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Deferred != nil {
		t.Errorf("PlanAction deferred: %+v", resp.Deferred)
	}
	return resp.Diagnostics
}

// Under terraform validate the census has unknown counts and expressions
// (variables, a data source): nothing is rejected.
func TestValidateCensusWithUnknowns(t *testing.T) {
	typ := actionType(t)
	num := tftypes.NewValue(tftypes.Number, tftypes.UnknownValue)
	d := validate(t, map[string]tftypes.Value{
		"api_version": s("v1"), "kind": s("Node"), "label_selector": unknownString,
		"min_matching": num, "max_matching": num, "require_all": tftypes.NewValue(tftypes.Bool, true),
		"success_conditions": condList(t, typ, "success_conditions", "Ready", "True"),
		"expression":         unknownString, "set_expression": unknownString,
		"timeout": unknownString, "settle": s("20s"), "poll_interval": s("10s"),
	})
	if len(d) != 0 {
		t.Errorf("diagnostics:\n%s", diagText(d))
	}
}

func TestValidateRejects(t *testing.T) {
	d := validate(t, map[string]tftypes.Value{
		"api_version": s("v1"), "kind": s("Pod"), "namespace": s("ns"), "name": s("p"),
		"label_selector": s("app=x"), "timeout": s("10m"),
	})
	if len(d) != 1 || d[0].Severity != tfprotov6.DiagnosticSeverityError || d[0].Attribute.String() != `AttributeName("label_selector")` {
		t.Errorf("diagnostics:\n%s", diagText(d))
	}
	typ := actionType(t)
	d = validate(t, map[string]tftypes.Value{
		"api_version": s("v1"), "kind": s("Pod"), "timeout": s("10m"),
		"success_conditions": condList(t, typ, "success_conditions", "Ready", "True", "", "True"),
	})
	if len(d) != 1 || !strings.Contains(d[0].Attribute.String(), `ElementKeyInt(1).AttributeName("type")`) {
		t.Errorf("diagnostics:\n%s", diagText(d))
	}
}

// Warnings come from ValidateActionConfig only; PlanAction repeats errors,
// since Terraform calls both during a plan.
func TestWarningsNotRepeatedAtPlan(t *testing.T) {
	vals := map[string]tftypes.Value{
		"api_version": s("v1"), "kind": s("Pod"), "namespace": s("ns"), "name": s("p"),
		"require_all": tftypes.NewValue(tftypes.Bool, true), "timeout": s("10m"),
	}
	if d := validate(t, vals); len(d) != 1 || d[0].Severity != tfprotov6.DiagnosticSeverityWarning {
		t.Errorf("validate:\n%s", diagText(d))
	}
	if d := plan(t, vals); len(d) != 0 {
		t.Errorf("plan:\n%s", diagText(d))
	}
}

// A violation that only becomes visible once a variable is known is caught
// by PlanAction.
func TestPlanCatchesKnownViolation(t *testing.T) {
	vals := map[string]tftypes.Value{
		"api_version": s("v1"), "kind": s("Pod"), "namespace": s("ns"),
		"min_matching": n(0), "max_matching": n(0), "timeout": s("10m"), "settle": unknownString,
	}
	if d := validate(t, vals); len(d) != 0 {
		t.Errorf("validate:\n%s", diagText(d))
	}
	vals["settle"] = s("10m")
	if d := plan(t, vals); len(d) != 1 || !strings.Contains(d[0].Detail, "must be shorter than timeout") {
		t.Errorf("plan:\n%s", diagText(d))
	}
}

func configure(t *testing.T, srv server, vals map[string]tftypes.Value) {
	t.Helper()
	sch, err := srv.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	typ := sch.Provider.Block.ValueType().(tftypes.Object)
	resp, err := srv.ConfigureProvider(context.Background(), &tfprotov6.ConfigureProviderRequest{Config: objectValue(t, typ, vals)})
	if err != nil || len(resp.Diagnostics) != 0 {
		t.Fatalf("configure: %v\n%s", err, diagText(resp.Diagnostics))
	}
}

func invoke(t *testing.T, srv server, vals map[string]tftypes.Value) (progress []string, diags []*tfprotov6.Diagnostic) {
	t.Helper()
	resp, err := srv.InvokeAction(context.Background(), &tfprotov6.InvokeActionRequest{
		ActionType: "kubewait_condition", Config: objectValue(t, actionType(t), vals)})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range resp.Events {
		switch e := ev.Type.(type) {
		case tfprotov6.ProgressInvokeActionEventType:
			progress = append(progress, e.Message)
		case tfprotov6.CompletedInvokeActionEventType:
			diags = append(diags, e.Diagnostics...)
		}
	}
	return progress, diags
}

var drainVals = map[string]tftypes.Value{
	"api_version": s("v1"), "kind": s("Pod"), "namespace": s("ns"),
	"min_matching": n(0), "max_matching": n(0), "timeout": s("10m"),
}

// Fail closed: with nothing configured outside a pod, or a configuration
// that was unknown when the provider was configured, a wait errors instead
// of observing localhost.
func TestInvokeWithoutCluster(t *testing.T) {
	noEnv(t)
	srv := protoServer(t)
	configure(t, srv, nil)
	if _, d := invoke(t, srv, drainVals); len(d) != 1 || !strings.Contains(d[0].Detail, "no cluster is configured") {
		t.Errorf("empty provider config:\n%s", diagText(d))
	}

	srv = protoServer(t)
	configure(t, srv, map[string]tftypes.Value{"host": unknownString})
	if _, d := invoke(t, srv, drainVals); len(d) != 1 || !strings.Contains(d[0].Detail, "was not known") {
		t.Errorf("unknown provider config:\n%s", diagText(d))
	}

	// Invalid configuration is rejected at invoke too: an engine may never
	// have called ValidateActionConfig or PlanAction.
	bad := map[string]tftypes.Value{}
	for k, v := range drainVals {
		bad[k] = v
	}
	bad["settle"] = s("1h")
	if _, d := invoke(t, srv, bad); len(d) != 1 || !strings.Contains(d[0].Detail, "must be shorter than timeout") {
		t.Errorf("invalid config at invoke:\n%s", diagText(d))
	}
}

func TestSendProgressReleasedByCancel(t *testing.T) {
	never := make(chan struct{})
	send := func(action.InvokeProgressEvent) { never <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sendProgress(ctx, send, "x")
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("returned while the send was blocked")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("still blocked after cancel")
	}
	sendProgress(context.Background(), nil, "x") // no-op
}
