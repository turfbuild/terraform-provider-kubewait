// Package provider is the kubewait Terraform provider: a kubernetes
// provider-compatible connection and the kubewait_condition action.
package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ provider.Provider            = &kubewaitProvider{}
	_ provider.ProviderWithActions = &kubewaitProvider{}
)

// New returns the provider factory.
func New(version string) func() provider.Provider {
	return func() provider.Provider { return &kubewaitProvider{version: version} }
}

type kubewaitProvider struct {
	version string
}

type providerModel struct {
	Host                  types.String `tfsdk:"host"`
	Username              types.String `tfsdk:"username"`
	Password              types.String `tfsdk:"password"`
	Insecure              types.Bool   `tfsdk:"insecure"`
	TLSServerName         types.String `tfsdk:"tls_server_name"`
	ClientCertificate     types.String `tfsdk:"client_certificate"`
	ClientKey             types.String `tfsdk:"client_key"`
	ClusterCACertificate  types.String `tfsdk:"cluster_ca_certificate"`
	ConfigPath            types.String `tfsdk:"config_path"`
	ConfigPaths           types.List   `tfsdk:"config_paths"`
	ConfigContext         types.String `tfsdk:"config_context"`
	ConfigContextAuthInfo types.String `tfsdk:"config_context_auth_info"`
	ConfigContextCluster  types.String `tfsdk:"config_context_cluster"`
	Token                 types.String `tfsdk:"token"`
	ProxyURL              types.String `tfsdk:"proxy_url"`
	Exec                  []execModel  `tfsdk:"exec"`
}

type execModel struct {
	APIVersion types.String `tfsdk:"api_version"`
	Command    types.String `tfsdk:"command"`
	Args       types.List   `tfsdk:"args"`
	Env        types.Map    `tfsdk:"env"`
}

func (p *kubewaitProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "kubewait"
	resp.Version = p.version
}

func (p *kubewaitProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	str := func(desc string, sensitive bool, vs ...validator.String) schema.StringAttribute {
		return schema.StringAttribute{Optional: true, Sensitive: sensitive, Description: desc, Validators: vs}
	}
	resp.Schema = schema.Schema{
		Description: "Waits on Kubernetes object state. The connection attributes are the kubernetes provider's, " +
			"so a provider \"kubernetes\" block's connection settings copy over verbatim. Unlike that provider, " +
			"kubewait never falls back to localhost or in-cluster credentials: with nothing configured, a wait fails.",
		Attributes: map[string]schema.Attribute{
			"host":     str("The hostname (in form of URI) of the Kubernetes API server. Can be set with KUBE_HOST.", false),
			"username": str("The username for HTTP basic authentication. Can be set with KUBE_USER.", false),
			"password": str("The password for HTTP basic authentication. Can be set with KUBE_PASSWORD.", true),
			"insecure": schema.BoolAttribute{Optional: true,
				Description: "Whether the server should be accessed without verifying the TLS certificate. Can be set with KUBE_INSECURE."},
			"tls_server_name":        str("Server name for SNI and for checking the server certificate. Can be set with KUBE_TLS_SERVER_NAME.", false),
			"client_certificate":     str("PEM-encoded client certificate for TLS authentication. Can be set with KUBE_CLIENT_CERT_DATA.", false),
			"client_key":             str("PEM-encoded client certificate key for TLS authentication. Can be set with KUBE_CLIENT_KEY_DATA.", true),
			"cluster_ca_certificate": str("PEM-encoded root certificates bundle for TLS authentication. Can be set with KUBE_CLUSTER_CA_CERT_DATA.", false),
			"config_path": str("Path to the kube config file. Can be set with KUBE_CONFIG_PATH.", false,
				stringvalidator.ConflictsWith(path.MatchRoot("config_paths"))),
			"config_paths": schema.ListAttribute{Optional: true, ElementType: types.StringType,
				Description: "A list of paths to kube config files. Can be set with KUBE_CONFIG_PATHS."},
			"config_context":           str("The kubeconfig context to use. Can be set with KUBE_CTX.", false),
			"config_context_auth_info": str("The kubeconfig user to use. Can be set with KUBE_CTX_AUTH_INFO.", false),
			"config_context_cluster":   str("The kubeconfig cluster to use. Can be set with KUBE_CTX_CLUSTER.", false),
			"token":                    str("Token to authenticate a service account. Can be set with KUBE_TOKEN.", true),
			"proxy_url":                str("URL of the proxy to use for all API requests. Can be set with KUBE_PROXY_URL.", false),
		},
		Blocks: map[string]schema.Block{
			"exec": schema.ListNestedBlock{
				Description: "An exec-based credential plugin, such as `aws eks get-token`.",
				Validators:  []validator.List{listvalidator.SizeAtMost(1)},
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"api_version": schema.StringAttribute{Required: true,
							Description: "The client.authentication.k8s.io API version the plugin speaks, such as client.authentication.k8s.io/v1beta1."},
						"command": schema.StringAttribute{Required: true, Description: "The command to run."},
						"args":    schema.ListAttribute{Optional: true, ElementType: types.StringType, Description: "Arguments to the command."},
						"env":     schema.MapAttribute{Optional: true, ElementType: types.StringType, Description: "Environment variables for the command."},
					},
				},
			},
		},
	}
}

// Configure never contacts the cluster and never defers: a deferred
// provider configuration would defer every action's PlanAction. When any
// attribute is unknown (the cluster may not exist yet) the connection is
// left unresolved; Terraform configures again with known values before any
// action is invoked.
func (p *kubewaitProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	conn := &Connection{UserAgent: "terraform-provider-kubewait/" + p.version}
	resp.ActionData = conn
	if !req.Config.Raw.IsFullyKnown() {
		return
	}
	var m providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	s := &Settings{
		Host:                  m.Host.ValueString(),
		Username:              m.Username.ValueString(),
		Password:              m.Password.ValueString(),
		Insecure:              m.Insecure.ValueBool(),
		TLSServerName:         m.TLSServerName.ValueString(),
		ClientCertificate:     m.ClientCertificate.ValueString(),
		ClientKey:             m.ClientKey.ValueString(),
		ClusterCACertificate:  m.ClusterCACertificate.ValueString(),
		ConfigContext:         m.ConfigContext.ValueString(),
		ConfigContextAuthInfo: m.ConfigContextAuthInfo.ValueString(),
		ConfigContextCluster:  m.ConfigContextCluster.ValueString(),
		Token:                 m.Token.ValueString(),
		ProxyURL:              m.ProxyURL.ValueString(),
	}
	if v := m.ConfigPath.ValueString(); v != "" {
		s.ConfigPaths = []string{v}
	} else if !m.ConfigPaths.IsNull() {
		resp.Diagnostics.Append(m.ConfigPaths.ElementsAs(ctx, &s.ConfigPaths, false)...)
	}
	if m.Insecure.IsNull() {
		b, err := insecureFromEnv(getenv)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("insecure"), "Invalid KUBE_INSECURE", err.Error())
		}
		s.Insecure = b
	}
	if len(m.Exec) == 1 {
		e := m.Exec[0]
		s.Exec = &ExecSettings{APIVersion: e.APIVersion.ValueString(), Command: e.Command.ValueString()}
		if !e.Args.IsNull() {
			resp.Diagnostics.Append(e.Args.ElementsAs(ctx, &s.Exec.Args, false)...)
		}
		if !e.Env.IsNull() {
			resp.Diagnostics.Append(e.Env.ElementsAs(ctx, &s.Exec.Env, false)...)
		}
	}
	if err := s.envDefaults(getenv); err != nil {
		resp.Diagnostics.AddError("Invalid kubewait provider environment", err.Error())
	}
	if resp.Diagnostics.HasError() {
		return
	}
	conn.Settings = s
}

func (p *kubewaitProvider) Actions(context.Context) []func() action.Action {
	return []func() action.Action{NewConditionAction}
}

func (p *kubewaitProvider) Resources(context.Context) []func() resource.Resource { return nil }

func (p *kubewaitProvider) DataSources(context.Context) []func() datasource.DataSource { return nil }
