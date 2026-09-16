// -------------------------------------------------------------------------------
// Resource - User
//
// Author: Alex Freidah
//
// An identity credentials prove and grants empower. The id the orchestrator
// generates is what every credential and grant references, so it never moves:
// a rename changes only the name an operator reads.
//
// That is also why the name is updatable rather than replacing the resource.
// Replacement would destroy the user first, which the orchestrator refuses
// while it still holds a credential or a grant, so a rename modelled that way
// would fail on every user worth having.
// -------------------------------------------------------------------------------

package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/afreidah/s3-orchestrator/terraform/terraform-provider-s3-orchestrator/internal/client"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// userResource manages one identity.
type userResource struct {
	api *client.Client
}

// userModel is the resource's state, one field per schema attribute.
type userModel struct {
	ID   types.String `tfsdk:"id"`
	Name types.String `tfsdk:"name"`
}

// The framework checks at compile time that every method it needs is present.
var (
	_ resource.Resource                = &userResource{}
	_ resource.ResourceWithConfigure   = &userResource{}
	_ resource.ResourceWithImportState = &userResource{}
)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// NewUserResource returns the factory the provider registers.
func NewUserResource() resource.Resource {
	return &userResource{}
}

// Metadata names the resource type as it is written in HCL.
func (r *userResource) Metadata(
	_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_user"
}

// Schema declares what the resource carries.
func (r *userResource) Schema(
	_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse,
) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An identity that credentials prove and grants empower. " +
			"A user holding no grants authenticates and reaches nothing, which is a " +
			"safe state to create one in.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Generated identifier. Credentials and grants " +
					"reference it, and a rename does not move it.",
				PlanModifiers: []planmodifier.String{
					// The id survives a rename, so holding it across a plan keeps
					// everything referencing it from going unknown for no reason.
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Name an operator reads the identity by.",
			},
		},
	}
}

// Configure takes the API client the provider built.
//
// Terraform calls this during validation too, before the provider is
// configured, so no provider data is an ordinary state rather than a fault.
func (r *userResource) Configure(
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

// Create declares the identity and records the id the orchestrator generated.
func (r *userResource) Create(
	ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse,
) {
	var plan userModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	out, err := r.api.CreateUser(ctx, plan.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Could not create user", err.Error())
		return
	}

	plan.ID = types.StringValue(out.UserID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read reports what the orchestrator holds, which is what Terraform diffs the
// configuration against.
//
// A user that is gone is removed from state rather than raised as an error: it
// is drift, and reporting it as drift is what lets the next plan recreate it.
func (r *userResource) Read(
	ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse,
) {
	var state userModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	user, found, err := r.api.User(ctx, state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Could not read user", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	if user.Source == client.SourceConfig {
		resp.Diagnostics.AddError(
			"User is declared in the orchestrator's configuration file",
			fmt.Sprintf("The identity %q comes from config.yaml, which the admin API will not "+
				"change. Manage it by editing that file and sending SIGHUP, or remove it from "+
				"Terraform state.", state.ID.ValueString()),
		)
		return
	}

	state.Name = types.StringValue(user.Name)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update renames the identity in place, leaving the id alone.
func (r *userResource) Update(
	ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse,
) {
	var plan, state userModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.api.RenameUser(ctx, state.ID.ValueString(), plan.Name.ValueString()); err != nil {
		resp.Diagnostics.AddError("Could not rename user", err.Error())
		return
	}

	plan.ID = state.ID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete removes the identity.
//
// The orchestrator refuses while the user still holds a credential or a grant.
// Terraform destroys dependents first, so a set it manages whole comes apart in
// order; one created outside it surfaces here naming what is in the way.
func (r *userResource) Delete(
	ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse,
) {
	var state userModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.api.DeleteUser(ctx, state.ID.ValueString()); err != nil {
		resp.Diagnostics.AddError("Could not delete user", err.Error())
	}
}

// ImportState adopts an existing identity by its id.
//
// Only the id is written: Terraform calls Read immediately afterwards, which
// fills in everything else from the orchestrator.
func (r *userResource) ImportState(
	ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse,
) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
