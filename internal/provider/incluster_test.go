package provider

import (
	"net"
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// inClusterToken is where client-go looks for a pod's service account token.
const inClusterToken = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// TestInvokeInCluster runs inside a pod (make testincluster): an empty
// provider configuration falls back to the pod's service account, and a
// wait runs against the pod's own cluster.
func TestInvokeInCluster(t *testing.T) {
	if os.Getenv("KUBEWAIT_INCLUSTER") != "1" {
		t.Skip("set KUBEWAIT_INCLUSTER=1 inside a pod (make testincluster)")
	}

	cfg, err := (&Settings{}).RESTConfig("")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://" + net.JoinHostPort(os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT"))
	if cfg.Host != want || cfg.BearerTokenFile != inClusterToken {
		t.Errorf("host = %q, token file = %q; want %q, %q", cfg.Host, cfg.BearerTokenFile, want, inClusterToken)
	}

	srv := protoServer(t)
	configure(t, srv, nil)
	progress, d := invoke(t, srv, map[string]tftypes.Value{
		"api_version": s("v1"), "kind": s("Namespace"), "name": s("kube-system"),
		"expression": s("object.status.phase == 'Active'"), "timeout": s("1m"),
	})
	if len(d) != 0 {
		t.Errorf("invoke:\n%s", diagText(d))
	}
	for _, p := range progress {
		t.Log(p)
	}
}
