package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/turfbuild/terraform-provider-kubewait/internal/wait"
)

// conditionModel uses framework value types throughout, including inside
// lists: at validate and plan time any value may be unknown, which Go-native
// fields cannot hold.
type conditionModel struct {
	APIVersion        types.String `tfsdk:"api_version"`
	Kind              types.String `tfsdk:"kind"`
	Namespace         types.String `tfsdk:"namespace"`
	Name              types.String `tfsdk:"name"`
	LabelSelector     types.String `tfsdk:"label_selector"`
	FieldSelector     types.String `tfsdk:"field_selector"`
	Filter            types.String `tfsdk:"filter"`
	SuccessConditions types.List   `tfsdk:"success_conditions"`
	FailureConditions types.List   `tfsdk:"failure_conditions"`
	Expression        types.String `tfsdk:"expression"`
	FailureExpression types.String `tfsdk:"failure_expression"`
	SetExpression     types.String `tfsdk:"set_expression"`
	MinMatching       types.Number `tfsdk:"min_matching"`
	MaxMatching       types.Number `tfsdk:"max_matching"`
	RequireAll        types.Bool   `tfsdk:"require_all"`
	Absent            types.String `tfsdk:"absent"`
	Timeout           types.String `tfsdk:"timeout"`
	Settle            types.String `tfsdk:"settle"`
	PollInterval      types.String `tfsdk:"poll_interval"`
	Watch             types.Bool   `tfsdk:"watch"`
	ProgressFields    types.List   `tfsdk:"progress_fields"`
	ProgressInterval  types.String `tfsdk:"progress_interval"`
}

type conditionEntryModel struct {
	Type   types.String `tfsdk:"type"`
	Status types.String `tfsdk:"status"`
	Reason types.String `tfsdk:"reason"`
}

// parseConfig is the single parser behind ValidateConfig, ModifyPlan and
// Invoke: config → model → wait.Raw → wait.Parse.
func parseConfig(ctx context.Context, cfg tfsdk.Config) (*wait.Spec, wait.Problems, diag.Diagnostics) {
	var m conditionModel
	diags := cfg.Get(ctx, &m)
	if diags.HasError() {
		return nil, nil, diags
	}
	raw := wait.Raw{
		APIVersion:        str(m.APIVersion),
		Kind:              str(m.Kind),
		Namespace:         str(m.Namespace),
		Name:              str(m.Name),
		LabelSelector:     str(m.LabelSelector),
		FieldSelector:     str(m.FieldSelector),
		Filter:            str(m.Filter),
		Expression:        str(m.Expression),
		FailureExpression: str(m.FailureExpression),
		SetExpression:     str(m.SetExpression),
		MinMatching:       num(m.MinMatching),
		MaxMatching:       num(m.MaxMatching),
		RequireAll:        boolean(m.RequireAll),
		Absent:            str(m.Absent),
		Timeout:           str(m.Timeout),
		Settle:            str(m.Settle),
		PollInterval:      str(m.PollInterval),
		Watch:             boolean(m.Watch),
		ProgressInterval:  str(m.ProgressInterval),
	}
	var d diag.Diagnostics
	raw.SuccessConditions, d = conds(ctx, m.SuccessConditions)
	diags.Append(d...)
	raw.FailureConditions, d = conds(ctx, m.FailureConditions)
	diags.Append(d...)
	raw.ProgressFields = strs(m.ProgressFields)
	if diags.HasError() {
		return nil, nil, diags
	}
	spec, probs := wait.Parse(raw)
	return spec, probs, diags
}

func str(v types.String) wait.Str {
	return wait.Str{V: v.ValueString(), Set: !v.IsNull() && !v.IsUnknown(), Unknown: v.IsUnknown()}
}

func num(v types.Number) wait.Num {
	return wait.Num{V: v.ValueBigFloat(), Set: !v.IsNull() && !v.IsUnknown(), Unknown: v.IsUnknown()}
}

func boolean(v types.Bool) wait.Bool {
	return wait.Bool{V: v.ValueBool(), Set: !v.IsNull() && !v.IsUnknown(), Unknown: v.IsUnknown()}
}

func conds(ctx context.Context, v types.List) (wait.Conds, diag.Diagnostics) {
	if v.IsUnknown() {
		return wait.Conds{Unknown: true}, nil
	}
	if v.IsNull() {
		return wait.Conds{}, nil
	}
	out := wait.Conds{Set: true}
	var diags diag.Diagnostics
	for _, e := range v.Elements() {
		obj, ok := e.(types.Object)
		if !ok || obj.IsUnknown() {
			return wait.Conds{Unknown: true}, diags
		}
		var cm conditionEntryModel
		diags.Append(obj.As(ctx, &cm, basetypes.ObjectAsOptions{})...)
		out.Items = append(out.Items, wait.RawCondition{Type: str(cm.Type), Status: str(cm.Status), Reason: str(cm.Reason)})
	}
	return out, diags
}

func strs(v types.List) wait.Strs {
	if v.IsUnknown() {
		return wait.Strs{Unknown: true}
	}
	if v.IsNull() {
		return wait.Strs{}
	}
	out := wait.Strs{Set: true}
	for _, e := range v.Elements() {
		s, _ := e.(types.String)
		out.Items = append(out.Items, str(s))
	}
	return out
}

// addProblems reports validation findings as attribute diagnostics.
func addProblems(diags *diag.Diagnostics, probs wait.Problems, warnings bool) {
	for _, p := range probs {
		ap := path.Empty()
		for _, e := range p.Path {
			switch v := e.(type) {
			case string:
				ap = ap.AtName(v)
			case int:
				ap = ap.AtListIndex(v)
			}
		}
		switch {
		case p.Severity == wait.Error:
			diags.AddAttributeError(ap, p.Summary, p.Detail)
		case warnings:
			diags.AddAttributeWarning(ap, p.Summary, p.Detail)
		}
	}
}
