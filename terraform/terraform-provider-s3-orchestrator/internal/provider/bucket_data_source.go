// -------------------------------------------------------------------------------
// Data Source - Bucket
//
// Author: Alex Freidah
//
// Reads a virtual bucket the deployment already declares, from either source.
// This is the way to reference a bucket the configuration file owns: the
// resource refuses to manage one, but a grant still has to name it, and naming
// it through here fails the plan when it does not exist rather than at apply.
// -------------------------------------------------------------------------------

package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/afreidah/s3-orchestrator/terraform/terraform-provider-s3-orchestrator/internal/client"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// bucketDataSource reads one virtual bucket.
type bucketDataSource struct {
	api *client.Client
}

// bucketDataSourceModel is what the data source reports.
type bucketDataSourceModel struct {
	Name                types.String `tfsdk:"name"`
	MaxMultipartUploads types.Int64  `tfsdk:"max_multipart_uploads"`
	Source              types.String `tfsdk:"source"`
}

// The framework checks at compile time that every method it needs is present.
var (
	_ datasource.DataSource              = &bucketDataSource{}
	_ datasource.DataSourceWithConfigure = &bucketDataSource{}
)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// NewBucketDataSource returns the factory the provider registers.
func NewBucketDataSource() datasource.DataSource {
	return &bucketDataSource{}
}

// Metadata names the data source as it is written in HCL.
func (d *bucketDataSource) Metadata(
	_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_bucket"
}

// Schema declares what the data source reports.
func (d *bucketDataSource) Schema(
	_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse,
) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An existing virtual bucket, declared by either the " +
			"orchestrator's configuration file or its store.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Bucket name to look up.",
			},
			"max_multipart_uploads": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "Multipart uploads that may be in flight at once. " +
					"Zero is unlimited.",
			},
			"source": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Which of the two places declared it, `config` or " +
					"`store`. A `config` bucket cannot be managed as a resource.",
			},
		},
	}
}

// Configure takes the API client the provider built.
func (d *bucketDataSource) Configure(
	_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse,
) {
	if req.ProviderData == nil {
		return
	}
	api, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data",
			fmt.Sprintf("Expected *client.Client, got %T. This is a bug in the provider.", req.ProviderData),
		)
		return
	}
	d.api = api
}

// Read looks the bucket up by name.
//
// An absent bucket is an error rather than an empty result: a configuration
// referring to a bucket that does not exist is a mistake worth stopping on,
// and it is the reason to reach for this over a hardcoded string.
func (d *bucketDataSource) Read(
	ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse,
) {
	var config bucketDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	name := config.Name.ValueString()
	bucket, found, err := d.api.Bucket(ctx, name)
	if err != nil {
		resp.Diagnostics.AddError("Could not read bucket", err.Error())
		return
	}
	if !found {
		resp.Diagnostics.AddError(
			"Bucket not found",
			fmt.Sprintf("No bucket named %q is declared by this orchestrator.", name),
		)
		return
	}

	config.MaxMultipartUploads = types.Int64Value(int64(bucket.MaxMultipartUploads))
	config.Source = types.StringValue(bucket.Source)
	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
