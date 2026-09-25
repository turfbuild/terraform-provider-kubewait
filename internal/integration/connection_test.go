package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/turfbuild/terraform-provider-kubewait/internal/provider"
)

type server interface {
	tfprotov6.ProviderServer
	tfprotov6.ActionServer
}

func str(v string) tftypes.Value { return tftypes.NewValue(tftypes.String, v) }

// objectOf builds a value of typ from the given attributes, nulls elsewhere.
func objectOf(t *testing.T, typ tftypes.Object, vals map[string]tftypes.Value) *tfprotov6.DynamicValue {
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

// invokeDrain configures the provider as Terraform would, then invokes a
// drain on an empty namespace through the InvokeAction RPC: the whole
// provider path from connection attributes to verdict.
func invokeDrain(t *testing.T, providerVals func(tftypes.Object) map[string]tftypes.Value) ([]string, []*tfprotov6.Diagnostic) {
	t.Helper()
	ns := namespace(t, "conn")
	s, err := providerserver.NewProtocol6WithError(provider.New("test")())()
	if err != nil {
		t.Fatal(err)
	}
	srv := s.(server)
	ctx := context.Background()
	sch, err := srv.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	ptyp := sch.Provider.Block.ValueType().(tftypes.Object)
	cresp, err := srv.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{Config: objectOf(t, ptyp, providerVals(ptyp))})
	if err != nil || len(cresp.Diagnostics) > 0 {
		t.Fatalf("configure: %v %+v", err, cresp.Diagnostics)
	}
	atyp := sch.ActionSchemas["kubewait_condition"].Schema.Block.ValueType().(tftypes.Object)
	zero := tftypes.NewValue(tftypes.Number, big.NewFloat(0))
	iresp, err := srv.InvokeAction(ctx, &tfprotov6.InvokeActionRequest{ActionType: "kubewait_condition",
		Config: objectOf(t, atyp, map[string]tftypes.Value{
			"api_version": str("v1"), "kind": str("Pod"), "namespace": str(ns),
			"min_matching": zero, "max_matching": zero, "timeout": str("30s"),
		})})
	if err != nil {
		t.Fatal(err)
	}
	var progress []string
	var diags []*tfprotov6.Diagnostic
	for ev := range iresp.Events {
		switch e := ev.Type.(type) {
		case tfprotov6.ProgressInvokeActionEventType:
			progress = append(progress, e.Message)
		case tfprotov6.CompletedInvokeActionEventType:
			diags = append(diags, e.Diagnostics...)
		}
	}
	return progress, diags
}

func expectDrained(t *testing.T, progress []string, diags []*tfprotov6.Diagnostic) {
	t.Helper()
	expectDrainedOK(t, progress, diags)
}

func expectDrainedOK(t *testing.T, progress []string, diags []*tfprotov6.Diagnostic) {
	t.Helper()
	for _, d := range diags {
		t.Errorf("diagnostic: %s: %s", d.Summary, d.Detail)
	}
	if len(progress) == 0 || !strings.HasPrefix(progress[0], "success: no objects match") {
		t.Errorf("progress: %q", progress)
	}
}

func drained(t *testing.T, vals func(tftypes.Object) map[string]tftypes.Value) {
	t.Helper()
	p, d := invokeDrain(t, vals)
	expectDrained(t, p, d)
}

func pemOf(b []byte) tftypes.Value { return str(string(b)) }

func TestConnectionAttributes(t *testing.T) {
	requireEnv(t)
	drained(t, func(tftypes.Object) map[string]tftypes.Value {
		return map[string]tftypes.Value{
			"host":                   str(reader.Host),
			"cluster_ca_certificate": pemOf(reader.CAData),
			"client_certificate":     pemOf(reader.CertData),
			"client_key":             pemOf(reader.KeyData),
		}
	})
}

func writeKubeconfig(t *testing.T, cfg *rest.Config, context string) string {
	t.Helper()
	kc := clientcmdapi.NewConfig()
	kc.Clusters["envtest"] = &clientcmdapi.Cluster{Server: cfg.Host, CertificateAuthorityData: cfg.CAData}
	kc.AuthInfos["reader"] = &clientcmdapi.AuthInfo{ClientCertificateData: cfg.CertData, ClientKeyData: cfg.KeyData}
	kc.AuthInfos["nobody"] = &clientcmdapi.AuthInfo{Token: "nope"}
	kc.Contexts["good"] = &clientcmdapi.Context{Cluster: "envtest", AuthInfo: "reader"}
	kc.Contexts["bad"] = &clientcmdapi.Context{Cluster: "envtest", AuthInfo: "nobody"}
	kc.CurrentContext = context
	p := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*kc, p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConnectionConfigPath(t *testing.T) {
	requireEnv(t)
	p := writeKubeconfig(t, reader, "good")
	drained(t, func(tftypes.Object) map[string]tftypes.Value {
		return map[string]tftypes.Value{"config_path": str(p)}
	})
	// config_context overrides the file's current context.
	p = writeKubeconfig(t, reader, "bad")
	drained(t, func(tftypes.Object) map[string]tftypes.Value {
		return map[string]tftypes.Value{"config_path": str(p), "config_context": str("good")}
	})
}

// An exec credential plugin, such as `aws eks get-token`:
// host and CA from attributes, the client certificate from the plugin.
func TestConnectionExec(t *testing.T) {
	requireEnv(t)
	cred, _ := json.Marshal(map[string]any{
		"apiVersion": "client.authentication.k8s.io/v1beta1", "kind": "ExecCredential",
		"status": map[string]any{"clientCertificateData": string(reader.CertData), "clientKeyData": string(reader.KeyData)},
	})
	dir := t.TempDir()
	credFile := filepath.Join(dir, "cred.json")
	if err := os.WriteFile(credFile, cred, 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "get-token")
	// The plugin reads its credential from a file named by an env var the
	// exec block sets, and checks an argument, so both are proven to pass.
	body := fmt.Sprintf("#!/bin/sh\n[ \"$1\" = get-token ] || exit 3\ncat \"$KUBEWAIT_CRED\"\n")
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	drained(t, func(ptyp tftypes.Object) map[string]tftypes.Value {
		execList := ptyp.AttributeTypes["exec"].(tftypes.List)
		execObj := execList.ElementType.(tftypes.Object)
		return map[string]tftypes.Value{
			"host":                   str(reader.Host),
			"cluster_ca_certificate": pemOf(reader.CAData),
			"exec": tftypes.NewValue(execList, []tftypes.Value{tftypes.NewValue(execObj, map[string]tftypes.Value{
				"api_version": str("client.authentication.k8s.io/v1beta1"),
				"command":     str(script),
				"args":        tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{str("get-token")}),
				"env": tftypes.NewValue(tftypes.Map{ElementType: tftypes.String},
					map[string]tftypes.Value{"KUBEWAIT_CRED": str(credFile)}),
			})}),
		}
	})
}

// Why 401 gets one immediate retry: client-go's exec authenticator re-runs
// the plugin only on the request after a 401. Here the plugin's first
// credential is a token the server rejects (an EKS token that expired
// mid-wait); the retry picks up the refreshed credential and the wait
// carries on.
func TestConnectionExecRefreshAfter401(t *testing.T) {
	requireEnv(t)
	dir := t.TempDir()
	write := func(name string, v any) {
		b, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("stale.json", map[string]any{"apiVersion": "client.authentication.k8s.io/v1beta1", "kind": "ExecCredential",
		"status": map[string]any{"token": "expired-token"}})
	write("fresh.json", map[string]any{"apiVersion": "client.authentication.k8s.io/v1beta1", "kind": "ExecCredential",
		"status": map[string]any{"clientCertificateData": string(reader.CertData), "clientKeyData": string(reader.KeyData)}})
	script := filepath.Join(dir, "get-token")
	body := `#!/bin/sh
n=$(cat "$D/count" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$D/count"
if [ $n -eq 1 ]; then cat "$D/stale.json"; else cat "$D/fresh.json"; fi
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	drained(t, func(ptyp tftypes.Object) map[string]tftypes.Value {
		execList := ptyp.AttributeTypes["exec"].(tftypes.List)
		execObj := execList.ElementType.(tftypes.Object)
		return map[string]tftypes.Value{
			"host":                   str(reader.Host),
			"cluster_ca_certificate": pemOf(reader.CAData),
			"exec": tftypes.NewValue(execList, []tftypes.Value{tftypes.NewValue(execObj, map[string]tftypes.Value{
				"api_version": str("client.authentication.k8s.io/v1beta1"),
				"command":     str(script),
				"args":        tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, nil),
				"env": tftypes.NewValue(tftypes.Map{ElementType: tftypes.String},
					map[string]tftypes.Value{"D": str(dir)}),
			})}),
		}
	})
	n, _ := os.ReadFile(filepath.Join(dir, "count"))
	if strings.TrimSpace(string(n)) != "2" {
		t.Errorf("plugin ran %s times, want 2 (initial, then refresh after the 401)", strings.TrimSpace(string(n)))
	}
}
