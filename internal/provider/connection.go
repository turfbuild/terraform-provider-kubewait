package provider

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/util/homedir"
)

// ErrNoCluster is returned when nothing names a cluster. The kubernetes
// provider warns and carries on against an empty rest.Config (localhost);
// a wait must not, since a drain against the wrong cluster passes.
var ErrNoCluster = errors.New("no cluster is configured: set host (with credentials), config_path or config_paths, " +
	"or the matching KUBE_* environment variables. kubewait never falls back to localhost, ~/.kube/config or in-cluster credentials")

// Settings are the provider's connection attributes with environment
// defaults applied. Empty means unset, as in the kubernetes provider.
type Settings struct {
	Host                  string
	Username              string
	Password              string
	Insecure              bool
	TLSServerName         string
	ClientCertificate     string
	ClientKey             string
	ClusterCACertificate  string
	ConfigPaths           []string
	ConfigContext         string
	ConfigContextAuthInfo string
	ConfigContextCluster  string
	Token                 string
	ProxyURL              string
	Exec                  *ExecSettings
}

// ExecSettings is the exec block.
type ExecSettings struct {
	APIVersion string
	Command    string
	Args       []string
	Env        map[string]string
}

// envDefaults fills unset attributes from the kubernetes provider's
// environment variables.
func (s *Settings) envDefaults(getenv func(string) string) error {
	for _, d := range []struct {
		dst *string
		env string
	}{
		{&s.Host, "KUBE_HOST"},
		{&s.Username, "KUBE_USER"},
		{&s.Password, "KUBE_PASSWORD"},
		{&s.TLSServerName, "KUBE_TLS_SERVER_NAME"},
		{&s.ClientCertificate, "KUBE_CLIENT_CERT_DATA"},
		{&s.ClientKey, "KUBE_CLIENT_KEY_DATA"},
		{&s.ClusterCACertificate, "KUBE_CLUSTER_CA_CERT_DATA"},
		{&s.ConfigContext, "KUBE_CTX"},
		{&s.ConfigContextAuthInfo, "KUBE_CTX_AUTH_INFO"},
		{&s.ConfigContextCluster, "KUBE_CTX_CLUSTER"},
		{&s.Token, "KUBE_TOKEN"},
		{&s.ProxyURL, "KUBE_PROXY_URL"},
	} {
		if *d.dst == "" {
			*d.dst = getenv(d.env)
		}
	}
	if len(s.ConfigPaths) == 0 {
		if v := getenv("KUBE_CONFIG_PATH"); v != "" {
			s.ConfigPaths = []string{v}
		} else if v := getenv("KUBE_CONFIG_PATHS"); v != "" {
			s.ConfigPaths = filepath.SplitList(v)
		}
	}
	return nil
}

// insecureFromEnv reads KUBE_INSECURE for an unset insecure attribute.
func insecureFromEnv(getenv func(string) string) (bool, error) {
	v := getenv("KUBE_INSECURE")
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("KUBE_INSECURE: %w", err)
	}
	return b, nil
}

// RESTConfig resolves the settings the way the kubernetes provider's
// initializeConfiguration does: kubeconfig files only when named, the
// context overrides only with them, and explicit attributes over both.
// Unlike it, an empty configuration is an error (ErrNoCluster), and there
// is no in-cluster fallback.
func (s *Settings) RESTConfig(userAgent string) (*rest.Config, error) {
	overrides := &clientcmd.ConfigOverrides{}
	loader := &clientcmd.ClientConfigLoadingRules{}

	if len(s.ConfigPaths) > 0 {
		expanded := make([]string, 0, len(s.ConfigPaths))
		for _, p := range s.ConfigPaths {
			expanded = append(expanded, expandHome(p))
		}
		if len(expanded) == 1 {
			loader.ExplicitPath = expanded[0]
		} else {
			loader.Precedence = expanded
		}
		if s.ConfigContext != "" || s.ConfigContextAuthInfo != "" || s.ConfigContextCluster != "" {
			overrides.CurrentContext = s.ConfigContext
			overrides.Context = clientcmdapi.Context{AuthInfo: s.ConfigContextAuthInfo, Cluster: s.ConfigContextCluster}
		}
	}

	overrides.ClusterInfo.InsecureSkipTLSVerify = s.Insecure
	overrides.ClusterInfo.TLSServerName = s.TLSServerName
	if s.ClusterCACertificate != "" {
		overrides.ClusterInfo.CertificateAuthorityData = []byte(s.ClusterCACertificate)
	}
	if s.ClientCertificate != "" {
		overrides.AuthInfo.ClientCertificateData = []byte(s.ClientCertificate)
	}
	if s.Host != "" {
		// As in the kubernetes provider: the overrides are applied too late
		// for defaultServerUrlFor, so resolve the scheme here.
		hasCA := len(overrides.ClusterInfo.CertificateAuthorityData) != 0
		hasCert := len(overrides.AuthInfo.ClientCertificateData) != 0
		defaultTLS := (hasCA || hasCert) && !overrides.ClusterInfo.InsecureSkipTLSVerify
		u, _, err := rest.DefaultServerURL(s.Host, "", schema.GroupVersion{}, defaultTLS)
		if err != nil {
			return nil, fmt.Errorf("failed to parse host %q: %w", s.Host, err)
		}
		overrides.ClusterInfo.Server = u.String()
	}
	overrides.AuthInfo.Username = s.Username
	overrides.AuthInfo.Password = s.Password
	if s.ClientKey != "" {
		overrides.AuthInfo.ClientKeyData = []byte(s.ClientKey)
	}
	overrides.AuthInfo.Token = s.Token
	if s.Exec != nil {
		exec := &clientcmdapi.ExecConfig{
			InteractiveMode: clientcmdapi.IfAvailableExecInteractiveMode,
			APIVersion:      s.Exec.APIVersion,
			Command:         s.Exec.Command,
			Args:            s.Exec.Args,
		}
		for k, v := range s.Exec.Env {
			exec.Env = append(exec.Env, clientcmdapi.ExecEnvVar{Name: k, Value: v})
		}
		overrides.AuthInfo.Exec = exec
	}
	overrides.ClusterDefaults.ProxyURL = s.ProxyURL

	if len(s.ConfigPaths) == 0 && overrides.ClusterInfo.Server == "" {
		return nil, ErrNoCluster
	}

	// Load the named files, then resolve without the deferred loader's
	// in-cluster fallback.
	raw, err := loader.Load()
	if err != nil {
		return nil, err
	}
	cfg, err := clientcmd.NewNonInteractiveClientConfig(*raw, overrides.CurrentContext, overrides, loader).ClientConfig()
	if err != nil {
		if clientcmd.IsEmptyConfig(err) {
			return nil, ErrNoCluster
		}
		return nil, err
	}
	if cfg.Host == "" {
		return nil, ErrNoCluster
	}
	cfg.UserAgent = userAgent
	return cfg, nil
}

// Connection is what the provider hands its actions. Resolution is lazy:
// nothing touches kubeconfig files or the network until a wait runs.
type Connection struct {
	// Settings is nil when the provider configuration was not wholly known,
	// as when the cluster it names has not been created yet.
	Settings  *Settings
	UserAgent string
}

// RESTConfig resolves the connection for a wait.
func (c *Connection) RESTConfig() (*rest.Config, error) {
	if c.Settings == nil {
		return nil, errors.New("the provider configuration was not known when the provider was configured, " +
			"so there is no cluster to observe. Terraform configures providers with known values before apply; " +
			"an engine that invokes an action without doing so, for example a destroy-event action without the prior-state configuration, reaches this error instead of a wrong cluster")
	}
	return c.Settings.RESTConfig(c.UserAgent)
}

// expandHome expands a leading ~ as go-homedir (the kubernetes provider's
// choice) does.
func expandHome(p string) string {
	if p == "~" {
		return homedir.HomeDir()
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~"+string(filepath.Separator)) {
		return filepath.Join(homedir.HomeDir(), p[2:])
	}
	return p
}

// getenv is os.Getenv, replaceable in tests.
var getenv = os.Getenv
