// -------------------------------------------------------------------------------
// Provider - Configuration and Registration
//
// Author: Alex Freidah
//
// The provider block and the resources it offers. Terraform launches this
// binary as a subprocess and drives it over gRPC; this is the framework's side
// of that conversation.
//
// What the provider manages is exactly what the admin API manages. The
// orchestrator's configuration file stays authoritative for the buckets and
// credentials it declares and the API refuses to change those, so an entry from
// that source is neither importable nor manageable here.
// -------------------------------------------------------------------------------

package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/afreidah/s3-orchestrator/terraform/terraform-provider-s3-orchestrator/internal/client"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// The environment variables each provider attribute falls back to. They are the
// ones the admin CLI and the TUI already read, so a shell configured for one is
// configured for all three.
const (
	envAddr      = "S3O_ADMIN_ADDR"
	envAccessKey = "S3O_ACCESS_KEY_ID"
	envSecretKey = "S3O_SECRET_ACCESS_KEY" //nolint:gosec // G101: environment variable name
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// s3oProvider is the provider implementation.
type s3oProvider struct {
	version string
}

// providerModel is the provider block as configured.
type providerModel struct {
	Address         types.String `tfsdk:"address"`
	AccessKeyID     types.String `tfsdk:"access_key_id"`
	SecretAccessKey types.String `tfsdk:"secret_access_key"`
}

// attribute pairs one provider attribute with the environment variable behind
// it, so the two checks below read the same list rather than two copies.
type attribute struct {
	name       string
	configured types.String
	env        string
}

var _ provider.Provider = &s3oProvider{}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// New returns the factory the plugin server serves.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &s3oProvider{version: version}
	}
}

// Metadata names the provider, which every resource type is prefixed with.
func (p *s3oProvider) Metadata(
	_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse,
) {
	resp.TypeName = "s3orchestrator"
	resp.Version = p.version
}

// Schema describes the provider block.
//
// Every attribute is optional because each falls back to an environment
// variable. What is required is that a value reaches the client one way or the
// other, which Configure checks once both sources have been consulted.
func (p *s3oProvider) Schema(
	_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse,
) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the identities, keypairs and grants a running " +
			"s3-orchestrator authorizes requests against.\n\n" +
			"See [s3-orchestrator.munchbox.cc](https://s3-orchestrator.munchbox.cc) for " +
			"the project documentation, and the [Provisioning with Terraform]" +
			"(https://s3-orchestrator.munchbox.cc/guides/terraform-provider/) guide for " +
			"a walkthrough of onboarding a client with this provider.",
		Attributes: map[string]schema.Attribute{
			"address": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Base address of the admin API, for example " +
					"`https://s3.example.com`. A bare `host:port` is reached over HTTP. " +
					"Falls back to `" + envAddr + "`.",
			},
			"access_key_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Access key requests are signed with. Its user needs " +
					"the `admin-provision` permission on the orchestrator. Falls back to `" +
					envAccessKey + "`.",
			},
			"secret_access_key": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Secret half of the signing keypair. Falls back to `" +
					envSecretKey + "`.",
			},
		},
	}
}

// Configure builds the API client every resource is handed.
func (p *s3oProvider) Configure(
	ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse,
) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	attrs := []attribute{
		{"address", cfg.Address, envAddr},
		{"access_key_id", cfg.AccessKeyID, envAccessKey},
		{"secret_access_key", cfg.SecretAccessKey, envSecretKey},
	}
	// An unknown value is one Terraform cannot resolve until apply, usually
	// because it came from another resource. The client cannot be built from it,
	// so the run stops here naming the attribute rather than failing later
	// inside an unrelated resource.
	for _, a := range attrs {
		if a.configured.IsUnknown() {
			resp.Diagnostics.AddAttributeError(
				path.Root(a.name),
				"Provider configuration is not known until apply",
				"The "+a.name+" attribute cannot be resolved before the provider is configured. "+
					"Set it to a literal, or supply it through "+a.env+".",
			)
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}

	resolved := make(map[string]string, len(attrs))
	for _, a := range attrs {
		resolved[a.name] = valueOr(a.configured, a.env)
		if resolved[a.name] == "" {
			resp.Diagnostics.AddAttributeError(
				path.Root(a.name),
				"Missing provider configuration",
				"Set the "+a.name+" attribute on the provider block, or export "+a.env+".",
			)
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}

	api := client.New(resolved["address"], resolved["access_key_id"], resolved["secret_access_key"])
	resp.ResourceData = api
	resp.DataSourceData = api
}

// Resources lists what the provider manages.
//
// A bucket the configuration file declares stays read-only, as every other
// config-declared entry does; the resource reports that rather than failing as
// though the credential were at fault.
func (p *s3oProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewBucketResource,
		NewUserResource,
		NewCredentialResource,
		NewGrantResource,
	}
}

// DataSources lists what the provider reads.
//
// Backend state is absent deliberately: what the API reports about a backend is
// health, drain state and usage counters, which change between every plan. That
// is monitoring data, and holding a snapshot of it in Terraform state would
// describe a moment that has already passed.
func (p *s3oProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewBucketDataSource,
	}
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// valueOr resolves a configured value, falling back to an environment variable
// when the configuration left it out.
func valueOr(configured types.String, env string) string {
	if !configured.IsNull() && configured.ValueString() != "" {
		return configured.ValueString()
	}
	return os.Getenv(env)
}
