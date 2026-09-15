// -------------------------------------------------------------------------------
// Resource - Grant
//
// Author: Alex Freidah
//
// What one user reaches on one resource, and the permissions that reach
// carries. Written through the orchestrator's upsert, so an apply does not have
// to know whether the grant is already there and narrowing one replaces its
// permissions in place rather than leaving a window where the client reaches
// nothing.
//
// A grant has no identifier of its own: the orchestrator keys it by user, kind
// and name together. The identifier here is those three joined, which is also
// what an import names.
// -------------------------------------------------------------------------------

package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/afreidah/s3-orchestrator/terraform/terraform-provider-s3-orchestrator/internal/client"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// idSeparator joins the three parts that key a grant. A forward slash is safe
// because none of the three may contain one: a bucket name is rejected with a
// slash in it, a backend name comes from the configuration file, and a user id
// is generated.
const idSeparator = "/"

// idParts is how many segments an identifier carries. The orchestrator takes no
// name, so its identifier ends in an empty third segment rather than dropping
// it: one fixed shape means the parser has one case and its error can say
// exactly what was expected.
const idParts = 3

// defaultGrantKind is what a grant naming no kind is over. A bucket grant is
// the overwhelmingly common case, so the configuration that onboards a client
// names only the bucket.
const defaultGrantKind = "bucket"

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// grantResource manages what one user reaches on one resource.
type grantResource struct {
	api *client.Client
}

// grantModel is the resource's state, one field per schema attribute.
type grantModel struct {
	ID          types.String `tfsdk:"id"`
	UserID      types.String `tfsdk:"user_id"`
	Kind        types.String `tfsdk:"kind"`
	Name        types.String `tfsdk:"name"`
	Permissions types.Set    `tfsdk:"permissions"`
}

var (
	_ resource.Resource                = &grantResource{}
	_ resource.ResourceWithConfigure   = &grantResource{}
	_ resource.ResourceWithImportState = &grantResource{}
)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// NewGrantResource returns the factory the provider registers.
func NewGrantResource() resource.Resource {
	return &grantResource{}
}

// Metadata names the resource type as it is written in HCL.
func (r *grantResource) Metadata(
	_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_grant"
}

// Schema declares what the resource carries.
//
// Permissions are a set rather than a list: the orchestrator renders them in
// its own fixed order, so a list would report a diff every time the order the
// configuration happens to use differs from the one it reads back.
func (r *grantResource) Schema(
	_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse,
) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "What one user reaches on one resource. A named grant " +
			"replaces a wildcard for the resource it names rather than adding to it, " +
			"which is what lets broad access be carved down on one bucket.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The user, kind and name joined by `/`. A grant on " +
					"the orchestrator takes no name, so its identifier ends in an empty " +
					"segment, as in `user-abc123/orchestrator/`.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"user_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Identity the grant empowers.",
				PlanModifiers:       replace,
			},
			"kind": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "What the grant is over: `bucket` (the default), " +
					"`backend`, or `orchestrator`.",
				PlanModifiers: []planmodifier.String{
					// Computed as well as optional, so a configuration that names
					// no kind leaves it unknown on every later plan - and unknown
					// against the stored value would read as a change and replace
					// a grant nobody touched. Holding the stored value is what
					// keeps the default from churning.
					stringplanmodifier.UseStateForUnknown(),
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The one resource of that kind, or `*` for every one " +
					"of them including those added later. Omitted for the orchestrator, " +
					"which there is only one of.",
				PlanModifiers: replace,
			},
			"permissions": schema.SetAttribute{
				Required:    true,
				ElementType: types.StringType,
				// No plan modifier: changing what a grant carries is an update
				// rather than a replacement, because the orchestrator replaces
				// the set in place. Forcing replacement here would revoke and
				// re-grant, leaving a window where the client reaches nothing,
				// which is the thing the upsert exists to avoid.
				MarkdownDescription: "What the grant carries. A bucket takes `list-buckets`, " +
					"`list`, `read`, `write`, `delete` and `tags`, or `all`. A backend or the " +
					"orchestrator takes the `admin-` permissions, or `admin-all`. The two " +
					"vocabularies do not mix.",
			},
		},
	}
}

// Configure takes the API client the provider built.
func (r *grantResource) Configure(
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

// Create declares the grant.
func (r *grantResource) Create(
	ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse,
) {
	var plan grantModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.write(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read reports what the orchestrator holds.
//
// Grants are nested under the user that holds them, so one whose user is gone
// reads as absent: from here the grant is equally not there, and recreating it
// is what the next plan will do once the user is back.
func (r *grantResource) Read(
	ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse,
) {
	var state grantModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	kind, name := state.Kind.ValueString(), state.Name.ValueString()
	grant, found, err := r.api.Grant(ctx, state.UserID.ValueString(), kind, name)
	if err != nil {
		resp.Diagnostics.AddError("Could not read grant", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}

	permissions, diags := types.SetValueFrom(ctx, types.StringType, grant.Permissions)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	state.Permissions = permissions
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update replaces the permission set in place.
//
// Only the permissions reach here: every other attribute keys the grant, so
// changing one addresses a different grant and replaces the resource.
func (r *grantResource) Update(
	ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse,
) {
	var plan grantModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.write(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete withdraws the grant, leaving the user's others in place.
func (r *grantResource) Delete(
	ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse,
) {
	var state grantModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := r.api.DeleteGrant(ctx,
		state.UserID.ValueString(), state.Kind.ValueString(), state.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Could not withdraw grant", err.Error())
	}
}

// ImportState adopts an existing grant from its three parts.
//
// There is no identifier to pass through: the orchestrator keys a grant by user,
// kind and name together, so the identifier is those three and this is where it
// comes apart again.
func (r *grantResource) ImportState(
	ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse,
) {
	parts := strings.Split(req.ID, idSeparator)
	if len(parts) != idParts || parts[0] == "" || parts[1] == "" {
		resp.Diagnostics.AddError(
			"Unrecognised grant identifier",
			fmt.Sprintf("Expected user_id/kind/name, as in user-abc123/bucket/photos or "+
				"user-abc123/orchestrator/ for a grant the orchestrator takes no name for. Got %q.", req.ID),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("user_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("kind"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), optionalString(parts[2]))...)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// write declares the grant and fills in what the model did not carry.
//
// Create and Update send the same request, because the orchestrator's upsert
// makes them the same operation: one writes a grant that is not there, the
// other replaces one that is, and neither has to know which it was.
func (r *grantResource) write(ctx context.Context, plan *grantModel, diags *diag.Diagnostics) {
	var permissions []string
	diags.Append(plan.Permissions.ElementsAs(ctx, &permissions, false)...)
	if diags.HasError() {
		return
	}

	kind := plan.Kind.ValueString()
	if kind == "" {
		kind = defaultGrantKind
	}
	name := plan.Name.ValueString()

	if err := r.api.SetGrant(ctx, plan.UserID.ValueString(), kind, name, permissions); err != nil {
		diags.AddError("Could not declare grant", err.Error())
		return
	}

	plan.Kind = types.StringValue(kind)
	plan.ID = types.StringValue(strings.Join(
		[]string{plan.UserID.ValueString(), kind, name}, idSeparator))
}
