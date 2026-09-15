// -------------------------------------------------------------------------------
// Resource - Credential
//
// Author: Alex Freidah
//
// One keypair proving one user. Supplying both halves registers a credential
// the caller already holds, which is what lets this compose with a secret
// manager: the secret is generated wherever secrets are generated and recorded
// here, rather than minted by the orchestrator and pushed back out.
//
// Supplying neither mints one instead. That still works, but the minted secret
// crosses the wire exactly once and lives only in Terraform state afterwards,
// so losing state loses the credential.
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

// credentialResource manages one keypair.
type credentialResource struct {
	api *client.Client
}

// credentialModel is the resource's state, one field per schema attribute.
type credentialModel struct {
	ID              types.String `tfsdk:"id"`
	UserID          types.String `tfsdk:"user_id"`
	Label           types.String `tfsdk:"label"`
	AccessKeyID     types.String `tfsdk:"access_key_id"`
	SecretAccessKey types.String `tfsdk:"secret_access_key"`
}

// The framework calls ValidateConfig only on a resource that satisfies the
// interface for it, so the assertion is what keeps a signature typo from
// silently turning the check off.
var (
	_ resource.Resource                   = &credentialResource{}
	_ resource.ResourceWithConfigure      = &credentialResource{}
	_ resource.ResourceWithImportState    = &credentialResource{}
	_ resource.ResourceWithValidateConfig = &credentialResource{}
)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// NewCredentialResource returns the factory the provider registers.
func NewCredentialResource() resource.Resource {
	return &credentialResource{}
}

// Metadata names the resource type as it is written in HCL.
func (r *credentialResource) Metadata(
	_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_credential"
}

// Schema declares what the resource carries.
//
// Both halves of the keypair are optional and computed: optional so a caller
// holding one already can record it, computed so the orchestrator can mint one
// for a caller that does not. That pairing is also what stops Terraform
// planning to remove a minted keypair the configuration never mentioned.
func (r *credentialResource) Schema(
	_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse,
) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "A keypair proving one user. Several may name one user, " +
			"which is what lets one be rotated while the rest keep working.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The access key, which is what identifies the credential.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"user_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Identity this keypair proves.",
				PlanModifiers:       replace,
			},
			"label": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free text recording what holds this keypair.",
				PlanModifiers:       replace,
			},
			"access_key_id": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Access key to register. Omit it, with " +
					"`secret_access_key`, to have the orchestrator mint one instead.",
				PlanModifiers: replace,
			},
			"secret_access_key": schema.StringAttribute{
				Optional:  true,
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "Secret half of the keypair. Required alongside " +
					"`access_key_id`. Whether supplied or minted it is held in Terraform " +
					"state, which nothing reads it back out of the orchestrator to verify.",
				PlanModifiers: replace,
			},
		},
	}
}

// Configure takes the API client the provider built.
func (r *credentialResource) Configure(
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

// ValidateConfig refuses half a keypair before an apply is attempted.
//
// The orchestrator refuses it too, but a configuration error is worth reporting
// at plan time: a key with no secret cannot sign and a secret with no key names
// nothing, so either alone is a typo rather than a partial declaration.
func (r *credentialResource) ValidateConfig(
	ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse,
) {
	var cfg credentialModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if cfg.AccessKeyID.IsUnknown() || cfg.SecretAccessKey.IsUnknown() {
		return
	}
	if cfg.AccessKeyID.IsNull() == cfg.SecretAccessKey.IsNull() {
		return
	}
	resp.Diagnostics.AddAttributeError(
		path.Root("secret_access_key"),
		"Half a keypair",
		"Set access_key_id and secret_access_key together to register a keypair you "+
			"already hold, or leave both out to have one minted.",
	)
}

// Create records the keypair, minting one when the configuration supplied none.
func (r *credentialResource) Create(
	ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse,
) {
	var plan credentialModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	out, err := r.api.CreateCredential(ctx, client.CreateCredentialRequest{
		UserID:          plan.UserID.ValueString(),
		Label:           plan.Label.ValueString(),
		AccessKeyID:     plan.AccessKeyID.ValueString(),
		SecretAccessKey: plan.SecretAccessKey.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Could not create credential", err.Error())
		return
	}

	// The response echoes a supplied keypair and carries a minted one, so both
	// paths land here the same way and state always holds what was recorded.
	plan.ID = types.StringValue(out.AccessKeyID)
	plan.AccessKeyID = types.StringValue(out.AccessKeyID)
	plan.SecretAccessKey = types.StringValue(out.SecretAccessKey)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read confirms the keypair still exists and still proves the user it did.
//
// The secret is not part of the answer: the orchestrator never reads one back
// out, so state is the only record of it and this cannot detect a secret that
// was changed underneath Terraform. What it does detect is revocation, and a
// credential moved to another user.
func (r *credentialResource) Read(
	ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse,
) {
	var state credentialModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	cred, found, err := r.api.Credential(ctx, state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Could not read credential", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	if cred.Source == client.SourceConfig {
		resp.Diagnostics.AddError(
			"Credential is declared in the orchestrator's configuration file",
			fmt.Sprintf("The keypair %q comes from config.yaml, which the admin API will not "+
				"change. Manage it by editing that file and sending SIGHUP, or remove it from "+
				"Terraform state.", state.ID.ValueString()),
		)
		return
	}

	state.UserID = types.StringValue(cred.UserID)
	state.AccessKeyID = types.StringValue(cred.AccessKeyID)
	state.Label = optionalString(cred.Label)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is unreachable: every attribute replaces the resource, because a
// keypair cannot be changed in place. The method exists to satisfy the
// interface.
func (r *credentialResource) Update(
	_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse,
) {
	resp.Diagnostics.AddError(
		"Credentials cannot be updated",
		"Every attribute of a credential replaces it. This is a bug in the provider.",
	)
}

// Delete revokes the keypair, leaving its siblings working.
func (r *credentialResource) Delete(
	ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse,
) {
	var state credentialModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.api.DeleteCredential(ctx, state.ID.ValueString()); err != nil {
		resp.Diagnostics.AddError("Could not revoke credential", err.Error())
	}
}

// ImportState adopts an existing keypair by its access key.
//
// The secret cannot come with it: nothing reads one back out of the
// orchestrator. State therefore holds a null secret until the configuration
// supplies the one it already has, which is another reason to supply rather
// than mint.
func (r *credentialResource) ImportState(
	ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse,
) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// optionalString renders a value the API omits when empty, so an absent label
// reads as null rather than as the empty string a configuration never wrote.
func optionalString(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}
