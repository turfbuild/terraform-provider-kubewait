package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/utils/clock"

	"github.com/turfbuild/terraform-provider-kubewait/internal/wait"
	"github.com/turfbuild/terraform-provider-kubewait/internal/watcher"
)

var (
	_ action.Action                   = &conditionAction{}
	_ action.ActionWithConfigure      = &conditionAction{}
	_ action.ActionWithValidateConfig = &conditionAction{}
	_ action.ActionWithModifyPlan     = &conditionAction{}
)

// NewConditionAction returns the kubewait_condition action.
func NewConditionAction() action.Action { return &conditionAction{} }

type conditionAction struct {
	conn *Connection
}

func (a *conditionAction) Metadata(_ context.Context, req action.MetadataRequest, resp *action.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_condition"
}

// Schema matches testdata/dynamic_resources.json attribute for attribute
// (see schema_parity_test.go), so configurations type-checked against that
// schema carry over unchanged. Defaults live in code: action schemas have no
// computed attributes.
func (a *conditionAction) Schema(_ context.Context, _ action.SchemaRequest, resp *action.SchemaResponse) {
	optStr := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Optional: true, Description: desc}
	}
	condition := func(desc string) schema.ListNestedAttribute {
		return schema.ListNestedAttribute{
			Optional:    true,
			Description: desc,
			NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
				"type":   schema.StringAttribute{Required: true, Description: "The condition type, such as Ready or Succeeded."},
				"status": schema.StringAttribute{Required: true, Description: "The status to match, exactly: True, False or Unknown."},
				"reason": schema.StringAttribute{Optional: true, Description: "When set, the reason must match too."},
			}},
		}
	}
	resp.Schema = schema.Schema{
		Description: "Waits until Kubernetes objects reach a state: a single object's conditions, " +
			"or a count, set predicate or drain over a selected set. Read-only: it never creates, patches or deletes. " +
			"The wait ends at the first success or failure that holds for settle, or fails at timeout. " +
			"Failure wins ties. API errors are retried until timeout; 403 fails at once, and 401 after one immediate retry.",
		Attributes: map[string]schema.Attribute{
			"api_version": schema.StringAttribute{Required: true,
				Description: "Group/version of the kind to observe, such as v1 or nvcre.nvidia.com/v1alpha1."},
			"kind": schema.StringAttribute{Required: true, Description: "The kind to observe, such as Node, Certification or TrainJob."},
			"namespace": optStr("The namespace. Omit it for cluster-scoped kinds. For a namespaced kind in set mode, " +
				"omitting it selects all namespaces; single-object mode requires it for namespaced kinds."),
			"name":               optStr("Set for single-object mode: the wait observes this object. Unset: set mode."),
			"label_selector":     optStr("Server-side label selection (set mode)."),
			"field_selector":     optStr("Server-side field selection (set mode)."),
			"filter":             optStr("CEL over `object`: client-side narrowing of the selected set (set mode)."),
			"success_conditions": condition("Conditions that must all hold on an object for it to pass. Default: none."),
			"failure_conditions": condition("Conditions any one of which, holding on any matched object, is a failure. Default: none."),
			"expression":         optStr("CEL over `object`, ANDed with success_conditions."),
			"failure_expression": optStr("CEL over `object`, ORed with failure_conditions."),
			"set_expression":     optStr("CEL over `objects` (the matched set): a predicate on the whole set (set mode)."),
			"min_matching": schema.NumberAttribute{Optional: true,
				Description: "Minimum number of passing objects (set mode). Default: 1."},
			"max_matching": schema.NumberAttribute{Optional: true,
				Description: "Maximum number of passing objects (set mode). Default: unbounded. " +
					"max_matching = 0 (with min_matching = 0) and no success predicate is a drain: it succeeds when nothing matches."},
			"require_all": schema.BoolAttribute{Optional: true,
				Description: "Every matched object must pass, not just min_matching of them (set mode). Default: false."},
			"absent": optStr("Single-object mode: what a missing object counts as, pending, success or failure. Default: pending."),
			"timeout": schema.StringAttribute{Required: true,
				Description: "How long to wait, such as 30m. Expiry is a failure; nothing waits forever."},
			"settle": optStr("How long a verdict must hold continuously before it counts. Any change resets it. " +
				"Must be shorter than timeout. Default: 0s."),
			"poll_interval": optStr("Resync interval: a full re-list, and the only observation when watch is false. Default: 10s."),
			"watch":         schema.BoolAttribute{Optional: true, Description: "Watch between resyncs. Default: true."},
			"progress_fields": schema.ListAttribute{Optional: true, ElementType: types.StringType,
				Description: "Field paths reported for each observed object in progress events, such as status.conditions " +
					"or metadata.labels[\"app.kubernetes.io/name\"]. Default: none."},
			"progress_interval": optStr("Heartbeat interval between progress events when the verdict does not change. Default: 60s."),
		},
	}
}

// Configure receives the provider's connection. ProviderData is nil when
// Terraform validates without a configured provider.
func (a *conditionAction) Configure(_ context.Context, req action.ConfigureRequest, resp *action.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	conn, ok := req.ProviderData.(*Connection)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("expected *Connection, got %T", req.ProviderData))
		return
	}
	a.conn = conn
}

// ValidateConfig runs at validate (and plan) time, when variables and data
// sources may still be unknown: checks involving an unknown are skipped.
func (a *conditionAction) ValidateConfig(ctx context.Context, req action.ValidateConfigRequest, resp *action.ValidateConfigResponse) {
	_, probs, diags := parseConfig(ctx, req.Config)
	resp.Diagnostics.Append(diags...)
	addProblems(&resp.Diagnostics, probs, true)
}

// ModifyPlan repeats the checks once plan-time values are known, catching a
// violation that arrives through a variable. It never contacts the cluster
// and never defers. Warnings were already reported by ValidateConfig.
func (a *conditionAction) ModifyPlan(ctx context.Context, req action.ModifyPlanRequest, resp *action.ModifyPlanResponse) {
	_, probs, diags := parseConfig(ctx, req.Config)
	resp.Diagnostics.Append(diags...)
	addProblems(&resp.Diagnostics, probs, false)
}

func (a *conditionAction) Invoke(ctx context.Context, req action.InvokeRequest, resp *action.InvokeResponse) {
	// Validated again: an engine may invoke without ValidateActionConfig or
	// PlanAction ever having run.
	spec, probs, diags := parseConfig(ctx, req.Config)
	resp.Diagnostics.Append(diags...)
	addProblems(&resp.Diagnostics, probs, false)
	if resp.Diagnostics.HasError() {
		return
	}
	if spec == nil {
		resp.Diagnostics.AddError("Invalid kubewait_condition configuration", "The configuration was not wholly known at invoke time.")
		return
	}
	if a.conn == nil {
		resp.Diagnostics.AddError("kubewait provider not configured", "The action was invoked without a configured provider.")
		return
	}
	cfg, err := a.conn.RESTConfig()
	if err != nil {
		resp.Diagnostics.AddError("kubewait_condition has no cluster to observe", err.Error())
		return
	}
	// Names the cluster even when it came from in-cluster credentials.
	tflog.Debug(ctx, "kubewait_condition connection", map[string]any{"host": cfg.Host})
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		resp.Diagnostics.AddError("kubewait_condition cannot build a client", err.Error())
		return
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		resp.Diagnostics.AddError("kubewait_condition cannot build a client", err.Error())
		return
	}

	r := &watcher.Runner{
		Source:   watcher.DynamicSource{Client: dyn},
		Resolver: watcher.DiscoveryResolver{Client: disc},
		Clock:    clock.RealClock{},
		Progress: func(msg string) {
			tflog.Debug(ctx, "kubewait_condition progress", map[string]any{"message": msg})
			sendProgress(ctx, resp.SendProgress, msg)
		},
	}
	res := r.Run(ctx, spec)
	for _, w := range res.Warnings {
		resp.Diagnostics.AddWarning("kubewait_condition observed no objects", w)
	}
	if res.Verdict != wait.Success {
		resp.Diagnostics.AddError(res.Summary, res.Detail)
	}
}

// sendProgress sends one progress event without risking a hang. The
// framework's SendProgress is an unbuffered channel send, read by the
// InvokeAction stream loop; if Terraform goes away, that loop exits and the
// send would block forever. The send runs aside and is abandoned on ctx.
func sendProgress(ctx context.Context, send func(action.InvokeProgressEvent), msg string) {
	if send == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		send(action.InvokeProgressEvent{Message: msg})
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
