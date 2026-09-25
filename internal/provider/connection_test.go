package provider

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const kubeconfigA = `apiVersion: v1
kind: Config
current-context: a
clusters:
- name: cluster-a
  cluster: {server: "https://a.example:6443", insecure-skip-tls-verify: true}
- name: cluster-b
  cluster: {server: "https://b.example:6443", insecure-skip-tls-verify: true}
users:
- name: user-a
  user: {token: token-a}
- name: user-b
  user: {token: token-b}
contexts:
- name: a
  context: {cluster: cluster-a, user: user-a}
- name: b
  context: {cluster: cluster-b, user: user-b, namespace: other}
`

const kubeconfigC = `apiVersion: v1
kind: Config
clusters:
- name: cluster-c
  cluster: {server: "https://c.example:6443", insecure-skip-tls-verify: true}
users:
- name: user-c
  user: {token: token-c}
contexts:
- name: c
  context: {cluster: cluster-c, user: user-c}
`

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRESTConfigExplicitAttributes(t *testing.T) {
	cfg, err := (&Settings{Host: "https://api.example:443", Token: "t", ClusterCACertificate: "CA"}).RESTConfig("ua")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "https://api.example:443" || cfg.BearerToken != "t" || string(cfg.CAData) != "CA" || cfg.UserAgent != "ua" {
		t.Errorf("cfg = %+v", cfg)
	}
	// A bare host gets https when TLS material is present, http otherwise,
	// as in the kubernetes provider.
	cfg, _ = (&Settings{Host: "api.example:6443", ClusterCACertificate: "CA"}).RESTConfig("")
	if cfg.Host != "https://api.example:6443" {
		t.Errorf("host with CA = %s", cfg.Host)
	}
	cfg, _ = (&Settings{Host: "api.example:6443", ClusterCACertificate: "CA", Insecure: true}).RESTConfig("")
	if cfg.Host != "http://api.example:6443" {
		t.Errorf("insecure host = %s", cfg.Host)
	}
	cfg, _ = (&Settings{Host: "h", ClientCertificate: "CERT", ClientKey: "KEY", TLSServerName: "sni", ProxyURL: "http://proxy:3128"}).RESTConfig("")
	if string(cfg.CertData) != "CERT" || string(cfg.KeyData) != "KEY" || cfg.ServerName != "sni" || cfg.Proxy == nil {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestRESTConfigKubeconfig(t *testing.T) {
	a := writeFile(t, "a.yaml", kubeconfigA)

	cfg, err := (&Settings{ConfigPaths: []string{a}}).RESTConfig("")
	if err != nil || cfg.Host != "https://a.example:6443" || cfg.BearerToken != "token-a" {
		t.Fatalf("current context: %+v %v", cfg, err)
	}
	cfg, _ = (&Settings{ConfigPaths: []string{a}, ConfigContext: "b"}).RESTConfig("")
	if cfg.Host != "https://b.example:6443" || cfg.BearerToken != "token-b" {
		t.Errorf("config_context b: %+v", cfg)
	}
	cfg, _ = (&Settings{ConfigPaths: []string{a}, ConfigContextCluster: "cluster-b"}).RESTConfig("")
	if cfg.Host != "https://b.example:6443" || cfg.BearerToken != "token-a" {
		t.Errorf("config_context_cluster: %+v", cfg)
	}
	cfg, _ = (&Settings{ConfigPaths: []string{a}, ConfigContextAuthInfo: "user-b"}).RESTConfig("")
	if cfg.Host != "https://a.example:6443" || cfg.BearerToken != "token-b" {
		t.Errorf("config_context_auth_info: %+v", cfg)
	}
	// Explicit attributes win over the kubeconfig.
	cfg, _ = (&Settings{ConfigPaths: []string{a}, Host: "https://override:6443", Token: "t2"}).RESTConfig("")
	if cfg.Host != "https://override:6443" || cfg.BearerToken != "t2" {
		t.Errorf("override: %+v", cfg)
	}
	// config_paths merge; the first file's current-context applies.
	c := writeFile(t, "c.yaml", kubeconfigC)
	cfg, _ = (&Settings{ConfigPaths: []string{a, c}, ConfigContext: "c"}).RESTConfig("")
	if cfg.Host != "https://c.example:6443" || cfg.BearerToken != "token-c" {
		t.Errorf("merged: %+v", cfg)
	}
	// A named file that does not exist is an error.
	if _, err := (&Settings{ConfigPaths: []string{filepath.Join(t.TempDir(), "missing")}}).RESTConfig(""); err == nil {
		t.Error("missing kubeconfig accepted")
	}
	// An unknown context is an error, not a fallback.
	if _, err := (&Settings{ConfigPaths: []string{a}, ConfigContext: "nope"}).RESTConfig(""); err == nil {
		t.Error("unknown context accepted")
	}
}

func TestRESTConfigExec(t *testing.T) {
	cfg, err := (&Settings{Host: "https://eks:443", ClusterCACertificate: "CA", Exec: &ExecSettings{
		APIVersion: "client.authentication.k8s.io/v1beta1",
		Command:    "aws",
		Args:       []string{"eks", "get-token", "--cluster-name", "c", "--region", "us-east-1"},
		Env:        map[string]string{"AWS_PROFILE": "p"},
	}}).RESTConfig("")
	if err != nil {
		t.Fatal(err)
	}
	e := cfg.ExecProvider
	if e == nil || e.Command != "aws" || e.APIVersion != "client.authentication.k8s.io/v1beta1" || len(e.Args) != 6 ||
		len(e.Env) != 1 || e.Env[0].Name != "AWS_PROFILE" || e.InteractiveMode != clientcmdapi.IfAvailableExecInteractiveMode {
		t.Errorf("exec = %+v", e)
	}
}

func TestRESTConfigNoCluster(t *testing.T) {
	// Nothing configured: an error, never localhost or ~/.kube/config.
	t.Setenv("KUBECONFIG", writeFile(t, "k.yaml", kubeconfigA))
	for _, s := range []*Settings{{}, {Token: "t"}, {ConfigContext: "a"}} {
		if _, err := s.RESTConfig(""); !errors.Is(err, ErrNoCluster) {
			t.Errorf("%+v: err = %v", s, err)
		}
	}
	// A kubeconfig with no current context and no override.
	empty := writeFile(t, "e.yaml", "apiVersion: v1\nkind: Config\n")
	if _, err := (&Settings{ConfigPaths: []string{empty}}).RESTConfig(""); !errors.Is(err, ErrNoCluster) {
		t.Errorf("empty kubeconfig: err = %v", err)
	}
	// An unresolved (unknown at Configure) connection.
	if _, err := (&Connection{}).RESTConfig(); err == nil || !strings.Contains(err.Error(), "not known") {
		t.Errorf("unresolved: err = %v", err)
	}
}

func TestEnvDefaults(t *testing.T) {
	env := map[string]string{
		"KUBE_HOST":         "https://env:6443",
		"KUBE_TOKEN":        "env-token",
		"KUBE_CONFIG_PATHS": "/x" + string(filepath.ListSeparator) + "/y",
		"KUBE_CTX":          "ctx",
	}
	get := func(k string) string { return env[k] }

	s := &Settings{Token: "attr-token"}
	_ = s.envDefaults(get)
	if s.Host != "https://env:6443" || s.Token != "attr-token" || s.ConfigContext != "ctx" ||
		len(s.ConfigPaths) != 2 || s.ConfigPaths[1] != "/y" {
		t.Errorf("settings = %+v", s)
	}
	// KUBE_CONFIG_PATH beats KUBE_CONFIG_PATHS; an attribute beats both.
	env["KUBE_CONFIG_PATH"] = "/single"
	s = &Settings{}
	_ = s.envDefaults(get)
	if len(s.ConfigPaths) != 1 || s.ConfigPaths[0] != "/single" {
		t.Errorf("config paths = %v", s.ConfigPaths)
	}
	s = &Settings{ConfigPaths: []string{"/attr"}}
	_ = s.envDefaults(get)
	if len(s.ConfigPaths) != 1 || s.ConfigPaths[0] != "/attr" {
		t.Errorf("config paths = %v", s.ConfigPaths)
	}

	if b, err := insecureFromEnv(func(string) string { return "true" }); !b || err != nil {
		t.Errorf("KUBE_INSECURE=true: %v %v", b, err)
	}
	if _, err := insecureFromEnv(func(string) string { return "maybe" }); err == nil {
		t.Error("KUBE_INSECURE=maybe accepted")
	}
}

func TestExpandHome(t *testing.T) {
	home, _ := os.UserHomeDir()
	if got := expandHome("~/.kube/config"); got != filepath.Join(home, ".kube/config") {
		t.Errorf("expandHome = %s", got)
	}
	if got := expandHome("/abs/~x"); got != "/abs/~x" {
		t.Errorf("expandHome = %s", got)
	}
}
