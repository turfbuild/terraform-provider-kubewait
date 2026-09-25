package provider

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// jsonAttr is tfcoremock's dynamic_resources.json attribute format.
type jsonAttr struct {
	Type     string              `json:"type"`
	Required bool                `json:"required"`
	Optional bool                `json:"optional"`
	List     *jsonAttr           `json:"list"`
	Object   map[string]jsonAttr `json:"object"`
}

type server interface {
	tfprotov6.ProviderServer
	tfprotov6.ActionServer
}

func protoServer(t *testing.T) server {
	t.Helper()
	s, err := providerserver.NewProtocol6WithError(New("test")())()
	if err != nil {
		t.Fatal(err)
	}
	return s.(server)
}

func actionSchema(t *testing.T) *tfprotov6.SchemaBlock {
	t.Helper()
	resp, err := protoServer(t).GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range resp.Diagnostics {
		t.Errorf("schema diagnostic: %s: %s", d.Summary, d.Detail)
	}
	as, ok := resp.ActionSchemas["kubewait_condition"]
	if !ok {
		t.Fatalf("no kubewait_condition action; have %v", resp.ActionSchemas)
	}
	return as.Schema.Block
}

// The action schema is wire-identical (names, types, required/optional,
// nesting) to what tfcoremock 0.6.0-beta2 serves from
// testdata/dynamic_resources.json, so a configuration type-checked against
// that stand-in swaps providers without editing any wait. tfcoremock maps string/number/boolean to String/Number/Bool
// attributes, list(string) to a list attribute, and list(object) to a
// list-nested attribute (internal/schema/attribute.go ToTerraformAttribute).
func TestActionSchemaMatchesStandIn(t *testing.T) {
	raw, err := os.ReadFile("testdata/dynamic_resources.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]struct {
		Attributes map[string]jsonAttr `json:"attributes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	want := doc["kubewait_condition"].Attributes
	got := actionSchema(t)

	if len(got.BlockTypes) != 0 {
		t.Errorf("unexpected blocks: %v", got.BlockTypes)
	}
	compareAttrs(t, "", want, got.Attributes)
}

func compareAttrs(t *testing.T, prefix string, want map[string]jsonAttr, got []*tfprotov6.SchemaAttribute) {
	t.Helper()
	gotByName := map[string]*tfprotov6.SchemaAttribute{}
	var gotNames, wantNames []string
	for _, a := range got {
		gotByName[a.Name] = a
		gotNames = append(gotNames, a.Name)
	}
	for n := range want {
		wantNames = append(wantNames, n)
	}
	sort.Strings(gotNames)
	sort.Strings(wantNames)
	if len(gotNames) != len(wantNames) {
		t.Errorf("%sattributes = %v, want %v", prefix, gotNames, wantNames)
	}
	for name, w := range want {
		g, ok := gotByName[name]
		if !ok {
			t.Errorf("%s%s missing", prefix, name)
			continue
		}
		if g.Required != w.Required || g.Optional != w.Optional || g.Computed || g.Sensitive {
			t.Errorf("%s%s: required=%v optional=%v computed=%v sensitive=%v; want required=%v optional=%v",
				prefix, name, g.Required, g.Optional, g.Computed, g.Sensitive, w.Required, w.Optional)
		}
		if w.Type == "list" && w.List.Type == "object" {
			if g.NestedType == nil || g.NestedType.Nesting != tfprotov6.SchemaObjectNestingModeList {
				t.Errorf("%s%s: want a list-nested attribute, got type %v nested %v", prefix, name, g.Type, g.NestedType)
				continue
			}
			compareAttrs(t, prefix+name+".", w.List.Object, g.NestedType.Attributes)
			continue
		}
		if wt := wireType(w); g.Type == nil || !g.Type.Equal(wt) {
			t.Errorf("%s%s: type %v, want %v", prefix, name, g.Type, wt)
		}
	}
}

func wireType(a jsonAttr) tftypes.Type {
	switch a.Type {
	case "string":
		return tftypes.String
	case "number":
		return tftypes.Number
	case "boolean":
		return tftypes.Bool
	case "list":
		return tftypes.List{ElementType: wireType(*a.List)}
	}
	return nil
}
