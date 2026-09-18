// -------------------------------------------------------------------------------
// Resource - Bucket
//
// Author: Alex Freidah
//
// A virtual bucket: the namespace the orchestrator accepts writes under. The
// backend bucket it maps onto is not created here, because which backends a
// deployment writes to is the operator's to configure.
//
// The name identifies the bucket and every object stored beneath it, so
// changing it replaces the resource. Everything else is updated in place: the
// orchestrator refuses to delete a bucket holding objects, so a limit modelled
// as replacement would fail on every bucket worth having.
//
// What the configuration declares is what the bucket ends up holding. Dropping
// a cors_rule block removes that rule rather than leaving it behind.
// -------------------------------------------------------------------------------

package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/afreidah/s3-orchestrator/terraform/terraform-provider-s3-orchestrator/internal/client"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// bucketResource manages one virtual bucket.
type bucketResource struct {
	api *client.Client
}

// bucketModel is the resource's state, one field per schema attribute.
type bucketModel struct {
	Name                types.String    `tfsdk:"name"`
	MaxMultipartUploads types.Int64     `tfsdk:"max_multipart_uploads"`
	CORSRules           []corsRuleModel `tfsdk:"cors_rule"`
}

// corsRuleModel is one browser-facing rule the bucket carries.
type corsRuleModel struct {
	AllowedOrigins types.List  `tfsdk:"allowed_origins"`
	AllowedMethods types.List  `tfsdk:"allowed_methods"`
	AllowedHeaders types.List  `tfsdk:"allowed_headers"`
	ExposeHeaders  types.List  `tfsdk:"expose_headers"`
	MaxAge         types.Int64 `tfsdk:"max_age"`
}

// The framework checks at compile time that every method it needs is present.
var (
	_ resource.Resource                = &bucketResource{}
	_ resource.ResourceWithConfigure   = &bucketResource{}
	_ resource.ResourceWithImportState = &bucketResource{}
)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// NewBucketResource returns the factory the provider registers.
func NewBucketResource() resource.Resource {
	return &bucketResource{}
}

// Metadata names the resource type as it is written in HCL.
func (r *bucketResource) Metadata(
	_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_bucket"
}

// Schema declares what the resource carries.
func (r *bucketResource) Schema(
	_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse,
) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A virtual bucket the orchestrator accepts writes under. " +
			"Creating one declares a namespace; it does not create a bucket on any " +
			"backend, which stays the operator's to configure.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Bucket name. It identifies every object stored " +
					"beneath it, so changing it replaces the bucket.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"max_multipart_uploads": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(0),
				MarkdownDescription: "Multipart uploads that may be in flight against " +
					"this bucket at once. Zero, the default, is unlimited.",
			},
		},
		Blocks: map[string]schema.Block{
			"cors_rule": schema.ListNestedBlock{
				MarkdownDescription: "A browser-facing CORS rule. The orchestrator " +
					"rejects a rule that cannot match anything, because it reads as " +
					"granting access the operator never gets.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"allowed_origins": schema.ListAttribute{
							Required:            true,
							ElementType:         types.StringType,
							MarkdownDescription: "Origins the rule answers for. `*` matches any.",
						},
						"allowed_methods": schema.ListAttribute{
							Required:            true,
							ElementType:         types.StringType,
							MarkdownDescription: "HTTP methods the rule permits.",
						},
						"allowed_headers": schema.ListAttribute{
							Optional:            true,
							ElementType:         types.StringType,
							MarkdownDescription: "Request headers a preflight may ask for.",
						},
						"expose_headers": schema.ListAttribute{
							Optional:            true,
							ElementType:         types.StringType,
							MarkdownDescription: "Response headers the browser may read.",
						},
						"max_age": schema.Int64Attribute{
							Optional: true,
							Computed: true,
							MarkdownDescription: "Seconds a browser may cache the preflight " +
								"answer. Zero, the default, sends no caching header at all.",
						},
					},
				},
			},
		},
	}
}

// Configure takes the API client the provider built.
//
// Terraform calls this during validation too, before the provider is
// configured, so no provider data is an ordinary state rather than a fault.
func (r *bucketResource) Configure(
	_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse,
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
	r.api = api
}

// Create declares the bucket.
func (r *bucketResource) Create(
	ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse,
) {
	var plan bucketModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	rules, diags := corsRulesToAPI(ctx, plan.CORSRules)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	_, err := r.api.CreateBucket(ctx, client.CreateBucketRequest{
		Name:                plan.Name.ValueString(),
		MaxMultipartUploads: int(plan.MaxMultipartUploads.ValueInt64()),
		CORS:                rules,
	})
	if err != nil {
		resp.Diagnostics.AddError("Could not create bucket", err.Error())
		return
	}

	resp.Diagnostics.Append(resolveCORSState(ctx, rules, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read reports what the orchestrator holds, which is what Terraform diffs the
// configuration against.
//
// A bucket that is gone is removed from state rather than raised as an error:
// it is drift, and reporting it as drift is what lets the next plan recreate it.
func (r *bucketResource) Read(
	ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse,
) {
	var state bucketModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	bucket, found, err := r.api.Bucket(ctx, state.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Could not read bucket", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	if bucket.Source == client.SourceConfig {
		resp.Diagnostics.AddError(
			"Bucket is declared in the orchestrator's configuration file",
			fmt.Sprintf("The bucket %q comes from config.yaml, which the admin API will not "+
				"change. Manage it by editing that file and sending SIGHUP, or remove it from "+
				"Terraform state.", state.Name.ValueString()),
		)
		return
	}

	rules, diags := corsRulesFromAPI(ctx, bucket.CORS)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	state.MaxMultipartUploads = types.Int64Value(int64(bucket.MaxMultipartUploads))
	state.CORSRules = rules
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update replaces what the bucket carries, leaving its name and its objects
// alone.
func (r *bucketResource) Update(
	ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse,
) {
	var plan bucketModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	rules, diags := corsRulesToAPI(ctx, plan.CORSRules)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := r.api.UpdateBucket(ctx, plan.Name.ValueString(), client.UpdateBucketRequest{
		MaxMultipartUploads: int(plan.MaxMultipartUploads.ValueInt64()),
		CORS:                rules,
	})
	if err != nil {
		resp.Diagnostics.AddError("Could not update bucket", err.Error())
		return
	}

	resp.Diagnostics.Append(resolveCORSState(ctx, rules, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete removes the bucket.
//
// The orchestrator refuses while the bucket holds an object or is named by a
// grant. Terraform destroys dependents first, so a set it manages whole comes
// apart in order; objects written by a client surface here as a refusal naming
// how many are in the way.
func (r *bucketResource) Delete(
	ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse,
) {
	var state bucketModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.api.DeleteBucket(ctx, state.Name.ValueString()); err != nil {
		resp.Diagnostics.AddError("Could not delete bucket", err.Error())
	}
}

// ImportState adopts an existing bucket by its name.
//
// Only the name is written: Terraform calls Read immediately afterwards, which
// fills in everything else from the orchestrator.
func (r *bucketResource) ImportState(
	ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse,
) {
	resource.ImportStatePassthroughID(ctx, path.Root("name"), req, resp)
}

// -------------------------------------------------------------------------
// INTERNAL
// -------------------------------------------------------------------------

// resolveCORSState writes the rules that were sent back over the model, so
// every computed field holds a known value.
//
// max_age is computed, which means a configuration that omits it plans as
// unknown. An unknown left in state fails the apply, and what resolves it is
// what the orchestrator was told, which is what it now holds.
func resolveCORSState(ctx context.Context, sent []client.CORSRule, model *bucketModel) diag.Diagnostics {
	resolved, diags := corsRulesFromAPI(ctx, sent)
	if diags.HasError() {
		return diags
	}
	model.CORSRules = resolved
	return diags
}

// corsRulesToAPI renders the configured rules as the API takes them.
func corsRulesToAPI(ctx context.Context, rules []corsRuleModel) ([]client.CORSRule, diag.Diagnostics) {
	var diags diag.Diagnostics
	if len(rules) == 0 {
		return nil, diags
	}
	out := make([]client.CORSRule, 0, len(rules))
	for i := range rules {
		rule := client.CORSRule{MaxAge: int(rules[i].MaxAge.ValueInt64())}
		diags.Append(stringsFromList(ctx, rules[i].AllowedOrigins, &rule.AllowedOrigins)...)
		diags.Append(stringsFromList(ctx, rules[i].AllowedMethods, &rule.AllowedMethods)...)
		diags.Append(stringsFromList(ctx, rules[i].AllowedHeaders, &rule.AllowedHeaders)...)
		diags.Append(stringsFromList(ctx, rules[i].ExposeHeaders, &rule.ExposeHeaders)...)
		out = append(out, rule)
	}
	return out, diags
}

// corsRulesFromAPI renders what the orchestrator holds as resource state.
//
// An absent optional list is written back as null rather than as an empty list,
// because null is what a configuration that omits the argument holds, and the
// two do not compare equal.
func corsRulesFromAPI(ctx context.Context, rules []client.CORSRule) ([]corsRuleModel, diag.Diagnostics) {
	var diags diag.Diagnostics
	if len(rules) == 0 {
		return nil, diags
	}
	out := make([]corsRuleModel, 0, len(rules))
	for i := range rules {
		rule := corsRuleModel{MaxAge: types.Int64Value(int64(rules[i].MaxAge))}
		rule.AllowedOrigins = listFromStrings(ctx, rules[i].AllowedOrigins, false, &diags)
		rule.AllowedMethods = listFromStrings(ctx, rules[i].AllowedMethods, false, &diags)
		rule.AllowedHeaders = listFromStrings(ctx, rules[i].AllowedHeaders, true, &diags)
		rule.ExposeHeaders = listFromStrings(ctx, rules[i].ExposeHeaders, true, &diags)
		out = append(out, rule)
	}
	return out, diags
}

// stringsFromList reads a configured list into a plain slice. A null list reads
// as no elements, which is what omitting an optional argument means.
func stringsFromList(ctx context.Context, list types.List, out *[]string) diag.Diagnostics {
	if list.IsNull() || list.IsUnknown() {
		return nil
	}
	return list.ElementsAs(ctx, out, false)
}

// listFromStrings renders a slice as list state. optional says an empty slice
// becomes null rather than an empty list.
func listFromStrings(
	ctx context.Context, values []string, optional bool, diags *diag.Diagnostics,
) types.List {
	if len(values) == 0 && optional {
		return types.ListNull(types.StringType)
	}
	list, d := types.ListValueFrom(ctx, types.StringType, values)
	diags.Append(d...)
	return list
}
