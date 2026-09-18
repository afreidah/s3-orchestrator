// -------------------------------------------------------------------------------
// Bucket - Unit Tests
//
// Author: Alex Freidah
//
// Drives the resource and the data source against a stub orchestrator rather
// than a running one, so every branch an acceptance test reaches only on the
// happy path is covered here: a refused call, a bucket that has gone, a bucket
// the config file owns, and the CORS conversions on the way in and out.
//
// The stub answers the one listing endpoint the client reads and records the
// mutations, which is enough to drive all four methods.
// -------------------------------------------------------------------------------

package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dsschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/afreidah/s3-orchestrator/terraform/terraform-provider-s3-orchestrator/internal/client"
)

// -------------------------------------------------------------------------
// FIXTURES
// -------------------------------------------------------------------------

// storedBucket is what the stub reports for a bucket the store owns.
var storedBucket = client.Bucket{
	Name:                "photos",
	MaxMultipartUploads: 4,
	Source:              "store",
	CORS: []client.CORSRule{{
		AllowedOrigins: []string{"https://example.com"},
		AllowedMethods: []string{"GET"},
		MaxAge:         60,
	}},
}

// stub serves the provisioning listing and records what was asked of it.
type stub struct {
	buckets []client.Bucket
	status  int
	methods []string
	paths   []string
}

// handler answers the listing on GET and acknowledges every mutation.
func (s *stub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.methods = append(s.methods, r.Method)
		s.paths = append(s.paths, r.URL.Path)
		if s.status != 0 && r.Method != http.MethodGet {
			w.WriteHeader(s.status)
			return
		}
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(client.Provisioning{Buckets: s.buckets})
			return
		}
		_ = json.NewEncoder(w).Encode(client.OperationResponse{Status: "ok", Bucket: "photos"})
	}
}

// newBucketResource returns a resource wired to a stub, plus the stub.
func newBucketResource(t *testing.T, s *stub) *bucketResource {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return &bucketResource{api: client.New(srv.URL, "AKIATESTTESTTESTTEST", "secret")}
}

// newBucketDataSource returns a data source wired to a stub.
func newBucketDataSource(t *testing.T, s *stub) *bucketDataSource {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return &bucketDataSource{api: client.New(srv.URL, "AKIATESTTESTTESTTEST", "secret")}
}

// resourceSchema returns the resource's schema, which every plan and state a
// test builds has to carry.
func resourceSchema(t *testing.T) rschema.Schema {
	t.Helper()
	var resp resource.SchemaResponse
	(&bucketResource{}).Schema(context.Background(), resource.SchemaRequest{}, &resp)
	return resp.Schema
}

// dataSourceSchema returns the data source's schema.
func dataSourceSchema(t *testing.T) dsschema.Schema {
	t.Helper()
	var resp datasource.SchemaResponse
	(&bucketDataSource{}).Schema(context.Background(), datasource.SchemaRequest{}, &resp)
	return resp.Schema
}

// planOf renders a model as a plan the framework will read back.
func planOf(t *testing.T, m bucketModel) tfsdk.Plan {
	t.Helper()
	p := tfsdk.Plan{Schema: resourceSchema(t)}
	if diags := p.Set(context.Background(), m); diags.HasError() {
		t.Fatalf("building plan: %v", diags)
	}
	return p
}

// stateOf renders a model as resource state.
func stateOf(t *testing.T, m bucketModel) tfsdk.State {
	t.Helper()
	s := tfsdk.State{Schema: resourceSchema(t)}
	if diags := s.Set(context.Background(), m); diags.HasError() {
		t.Fatalf("building state: %v", diags)
	}
	return s
}

// emptyState is somewhere for a method under test to write.
func emptyState(t *testing.T) tfsdk.State {
	t.Helper()
	return tfsdk.State{Schema: resourceSchema(t)}
}

// configOf renders a model as data source configuration. Config carries no
// setter, so the value is built through a state holding the same schema.
func configOf(t *testing.T, m bucketDataSourceModel) tfsdk.Config {
	t.Helper()
	s := tfsdk.State{Schema: dataSourceSchema(t)}
	if diags := s.Set(context.Background(), m); diags.HasError() {
		t.Fatalf("building config: %v", diags)
	}
	return tfsdk.Config{Schema: dataSourceSchema(t), Raw: s.Raw}
}

// simpleModel is the smallest bucket a test can declare.
func simpleModel() bucketModel {
	return bucketModel{
		Name:                types.StringValue("photos"),
		MaxMultipartUploads: types.Int64Value(4),
	}
}

// -------------------------------------------------------------------------
// SCHEMA AND METADATA
// -------------------------------------------------------------------------

// TestBucketMetadata covers the type names both halves are written as.
func TestBucketMetadata(t *testing.T) {
	t.Parallel()

	var rresp resource.MetadataResponse
	(&bucketResource{}).Metadata(context.Background(),
		resource.MetadataRequest{ProviderTypeName: "s3orchestrator"}, &rresp)
	if rresp.TypeName != "s3orchestrator_bucket" {
		t.Errorf("resource type = %q, want s3orchestrator_bucket", rresp.TypeName)
	}

	var dresp datasource.MetadataResponse
	(&bucketDataSource{}).Metadata(context.Background(),
		datasource.MetadataRequest{ProviderTypeName: "s3orchestrator"}, &dresp)
	if dresp.TypeName != "s3orchestrator_bucket" {
		t.Errorf("data source type = %q, want s3orchestrator_bucket", dresp.TypeName)
	}
}

// TestBucketSchema covers the two declarations the resource's behaviour rests
// on: the name replaces the bucket, and the limit defaults rather than going
// unknown on a configuration that omits it.
func TestBucketSchema(t *testing.T) {
	t.Parallel()
	s := resourceSchema(t)

	name, ok := s.Attributes["name"].(rschema.StringAttribute)
	if !ok {
		t.Fatalf("name attribute = %T, want StringAttribute", s.Attributes["name"])
	}
	if len(name.PlanModifiers) == 0 {
		t.Error("name carries no plan modifier, so a rename would not replace the bucket")
	}

	limit, ok := s.Attributes["max_multipart_uploads"].(rschema.Int64Attribute)
	if !ok {
		t.Fatalf("limit attribute = %T, want Int64Attribute", s.Attributes["max_multipart_uploads"])
	}
	if limit.Default == nil {
		t.Error("the limit has no default, so omitting it would plan as unknown")
	}
	if _, ok := s.Blocks["cors_rule"]; !ok {
		t.Error("cors_rule block is absent")
	}

	if _, ok := dataSourceSchema(t).Attributes["source"]; !ok {
		t.Error("the data source does not report source, which is what marks a config bucket")
	}
}

// TestBucketFactories covers the two factories the provider registers, which
// nothing else calls without a running orchestrator.
func TestBucketFactories(t *testing.T) {
	t.Parallel()

	if _, ok := NewBucketResource().(*bucketResource); !ok {
		t.Error("NewBucketResource did not return a bucket resource")
	}
	if _, ok := NewBucketDataSource().(*bucketDataSource); !ok {
		t.Error("NewBucketDataSource did not return a bucket data source")
	}
}

// TestBucketImportState covers import writing the name and nothing else. Read
// runs immediately afterwards and fills in the rest, so writing more here would
// only be a guess at what the orchestrator holds.
func TestBucketImportState(t *testing.T) {
	t.Parallel()

	// Typed nulls rather than an empty state: import sets one attribute, and
	// setting one attribute needs a value that already carries the type.
	resp := resource.ImportStateResponse{State: stateOf(t, bucketModel{
		Name:                types.StringNull(),
		MaxMultipartUploads: types.Int64Null(),
	})}
	(&bucketResource{}).ImportState(context.Background(),
		resource.ImportStateRequest{ID: "photos"}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("diagnostics = %v, want none", resp.Diagnostics)
	}
	var got bucketModel
	if diags := resp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("reading state: %v", diags)
	}
	if got.Name.ValueString() != "photos" {
		t.Errorf("name = %q, want photos", got.Name.ValueString())
	}
}

// -------------------------------------------------------------------------
// CREATE
// -------------------------------------------------------------------------

// TestBucketCreate covers the declared bucket reaching the API and the plan
// landing in state.
func TestBucketCreate(t *testing.T) {
	t.Parallel()
	s := &stub{}
	r := newBucketResource(t, s)

	resp := resource.CreateResponse{State: emptyState(t)}
	r.Create(context.Background(),
		resource.CreateRequest{Plan: planOf(t, simpleModel())}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("diagnostics = %v, want none", resp.Diagnostics)
	}
	if len(s.methods) == 0 || s.methods[0] != http.MethodPost {
		t.Errorf("methods = %v, want a POST", s.methods)
	}
	var got bucketModel
	if diags := resp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("reading state: %v", diags)
	}
	if got.Name.ValueString() != "photos" {
		t.Errorf("state name = %q, want photos", got.Name.ValueString())
	}
}

// TestBucketCreateReportsRefusal covers the orchestrator saying no, which is
// what a duplicate name or an unreadable CORS rule arrives as.
func TestBucketCreateReportsRefusal(t *testing.T) {
	t.Parallel()
	s := &stub{status: http.StatusConflict}
	r := newBucketResource(t, s)

	resp := resource.CreateResponse{State: emptyState(t)}
	r.Create(context.Background(),
		resource.CreateRequest{Plan: planOf(t, simpleModel())}, &resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("diagnostics = none, want the refusal reported")
	}
	if summary := resp.Diagnostics.Errors()[0].Summary(); summary != "Could not create bucket" {
		t.Errorf("summary = %q, want %q", summary, "Could not create bucket")
	}
}

// -------------------------------------------------------------------------
// READ
// -------------------------------------------------------------------------

// TestBucketRead covers what the orchestrator holds replacing what state held.
func TestBucketRead(t *testing.T) {
	t.Parallel()
	s := &stub{buckets: []client.Bucket{storedBucket}}
	r := newBucketResource(t, s)

	resp := resource.ReadResponse{State: emptyState(t)}
	r.Read(context.Background(),
		resource.ReadRequest{State: stateOf(t, simpleModel())}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("diagnostics = %v, want none", resp.Diagnostics)
	}
	var got bucketModel
	if diags := resp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("reading state: %v", diags)
	}
	if got.MaxMultipartUploads.ValueInt64() != 4 {
		t.Errorf("limit = %d, want 4", got.MaxMultipartUploads.ValueInt64())
	}
	if len(got.CORSRules) != 1 || got.CORSRules[0].MaxAge.ValueInt64() != 60 {
		t.Errorf("cors = %+v, want the stored rule", got.CORSRules)
	}
}

// TestBucketReadDropsMissing covers a bucket that has gone being reported as
// drift rather than as an error, which is what lets the next plan recreate it.
func TestBucketReadDropsMissing(t *testing.T) {
	t.Parallel()
	s := &stub{}
	r := newBucketResource(t, s)

	resp := resource.ReadResponse{State: stateOf(t, simpleModel())}
	r.Read(context.Background(),
		resource.ReadRequest{State: stateOf(t, simpleModel())}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("diagnostics = %v, want none", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Error("state survived, want the resource dropped")
	}
}

// TestBucketReadRejectsConfigDeclared covers a bucket the config file owns
// saying so, rather than reading as a permissions failure.
func TestBucketReadRejectsConfigDeclared(t *testing.T) {
	t.Parallel()
	declared := storedBucket
	declared.Source = client.SourceConfig
	s := &stub{buckets: []client.Bucket{declared}}
	r := newBucketResource(t, s)

	resp := resource.ReadResponse{State: emptyState(t)}
	r.Read(context.Background(),
		resource.ReadRequest{State: stateOf(t, simpleModel())}, &resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("diagnostics = none, want the config declaration reported")
	}
	if detail := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(detail, "config.yaml") {
		t.Errorf("detail = %q, want it to name the config file", detail)
	}
}

// TestBucketReadReportsFailure covers an orchestrator that cannot be reached.
func TestBucketReadReportsFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	r := &bucketResource{api: client.New(srv.URL, "AKIATESTTESTTESTTEST", "secret")}

	resp := resource.ReadResponse{State: emptyState(t)}
	r.Read(context.Background(),
		resource.ReadRequest{State: stateOf(t, simpleModel())}, &resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("diagnostics = none, want the failure reported")
	}
}

// -------------------------------------------------------------------------
// UPDATE AND DELETE
// -------------------------------------------------------------------------

// TestBucketUpdate covers the rewrite going out as a PATCH and the plan landing
// in state.
func TestBucketUpdate(t *testing.T) {
	t.Parallel()
	s := &stub{buckets: []client.Bucket{storedBucket}}
	r := newBucketResource(t, s)

	plan := simpleModel()
	plan.MaxMultipartUploads = types.Int64Value(9)

	resp := resource.UpdateResponse{State: emptyState(t)}
	r.Update(context.Background(), resource.UpdateRequest{Plan: planOf(t, plan)}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("diagnostics = %v, want none", resp.Diagnostics)
	}
	if len(s.methods) == 0 || s.methods[0] != http.MethodPatch {
		t.Errorf("methods = %v, want a PATCH", s.methods)
	}
}

// TestBucketUpdateReportsRefusal covers the orchestrator refusing a rewrite.
func TestBucketUpdateReportsRefusal(t *testing.T) {
	t.Parallel()
	s := &stub{status: http.StatusBadRequest}
	r := newBucketResource(t, s)

	resp := resource.UpdateResponse{State: emptyState(t)}
	r.Update(context.Background(),
		resource.UpdateRequest{Plan: planOf(t, simpleModel())}, &resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("diagnostics = none, want the refusal reported")
	}
}

// TestBucketDelete covers removal reaching the API.
func TestBucketDelete(t *testing.T) {
	t.Parallel()
	s := &stub{}
	r := newBucketResource(t, s)

	var resp resource.DeleteResponse
	r.Delete(context.Background(),
		resource.DeleteRequest{State: stateOf(t, simpleModel())}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("diagnostics = %v, want none", resp.Diagnostics)
	}
	if len(s.methods) == 0 || s.methods[0] != http.MethodDelete {
		t.Errorf("methods = %v, want a DELETE", s.methods)
	}
}

// TestBucketDeleteReportsRefusal covers the orchestrator refusing while the
// bucket still holds objects.
func TestBucketDeleteReportsRefusal(t *testing.T) {
	t.Parallel()
	s := &stub{status: http.StatusConflict}
	r := newBucketResource(t, s)

	var resp resource.DeleteResponse
	r.Delete(context.Background(),
		resource.DeleteRequest{State: stateOf(t, simpleModel())}, &resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("diagnostics = none, want the refusal reported")
	}
}

// -------------------------------------------------------------------------
// DATA SOURCE
// -------------------------------------------------------------------------

// TestBucketDataSourceRead covers reading a bucket the config file declares,
// which is the case the resource will not manage.
func TestBucketDataSourceRead(t *testing.T) {
	t.Parallel()
	declared := storedBucket
	declared.Source = client.SourceConfig
	s := &stub{buckets: []client.Bucket{declared}}
	d := newBucketDataSource(t, s)

	cfg := configOf(t, bucketDataSourceModel{Name: types.StringValue("photos")})

	resp := datasource.ReadResponse{State: tfsdk.State{Schema: dataSourceSchema(t)}}
	d.Read(context.Background(), datasource.ReadRequest{Config: cfg}, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("diagnostics = %v, want none", resp.Diagnostics)
	}
	var got bucketDataSourceModel
	if diags := resp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("reading state: %v", diags)
	}
	if got.Source.ValueString() != client.SourceConfig {
		t.Errorf("source = %q, want config", got.Source.ValueString())
	}
}

// TestBucketDataSourceReadMissing covers a name nothing declares stopping the
// plan, which is the reason to reach for the data source over a literal.
func TestBucketDataSourceReadMissing(t *testing.T) {
	t.Parallel()
	d := newBucketDataSource(t, &stub{})

	cfg := configOf(t, bucketDataSourceModel{Name: types.StringValue("gone")})

	resp := datasource.ReadResponse{State: tfsdk.State{Schema: dataSourceSchema(t)}}
	d.Read(context.Background(), datasource.ReadRequest{Config: cfg}, &resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("diagnostics = none, want the absence reported")
	}
	if summary := resp.Diagnostics.Errors()[0].Summary(); summary != "Bucket not found" {
		t.Errorf("summary = %q, want %q", summary, "Bucket not found")
	}
}

// TestBucketDataSourceConfigure covers the two states the framework hands a
// data source: nothing yet, and the wrong thing.
func TestBucketDataSourceConfigure(t *testing.T) {
	t.Parallel()

	var absent datasource.ConfigureResponse
	(&bucketDataSource{}).Configure(context.Background(), datasource.ConfigureRequest{}, &absent)
	if absent.Diagnostics.HasError() {
		t.Errorf("diagnostics = %v, want none", absent.Diagnostics)
	}

	var wrong datasource.ConfigureResponse
	(&bucketDataSource{}).Configure(context.Background(),
		datasource.ConfigureRequest{ProviderData: "not a client"}, &wrong)
	if !wrong.Diagnostics.HasError() {
		t.Fatal("diagnostics = none, want an error")
	}
}

// -------------------------------------------------------------------------
// CORS CONVERSION
// -------------------------------------------------------------------------

// TestCORSRoundTrip covers a rule surviving the trip out to the API and back,
// and an omitted optional list coming back null rather than empty. Null is what
// a configuration that omits the argument holds, and the two do not compare
// equal, so an empty list here is a diff on every plan.
func TestCORSRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	from, diags := corsRulesFromAPI(ctx, []client.CORSRule{{
		AllowedOrigins: []string{"https://example.com"},
		AllowedMethods: []string{"GET", "HEAD"},
		MaxAge:         30,
	}})
	if diags.HasError() {
		t.Fatalf("corsRulesFromAPI: %v", diags)
	}
	if !from[0].AllowedHeaders.IsNull() {
		t.Error("allowed_headers is not null, so an omitted list would diff every plan")
	}

	back, diags := corsRulesToAPI(ctx, from)
	if diags.HasError() {
		t.Fatalf("corsRulesToAPI: %v", diags)
	}
	if len(back) != 1 || back[0].MaxAge != 30 || len(back[0].AllowedMethods) != 2 {
		t.Errorf("round trip = %+v, want the rule unchanged", back)
	}
	if len(back[0].AllowedHeaders) != 0 {
		t.Errorf("allowed_headers = %v, want none", back[0].AllowedHeaders)
	}
}

// TestCORSEmptyIsNil covers no rules staying no rules in both directions, which
// is what clears a bucket's rules rather than leaving them.
func TestCORSEmptyIsNil(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	if out, _ := corsRulesToAPI(ctx, nil); out != nil {
		t.Errorf("corsRulesToAPI(nil) = %v, want nil", out)
	}
	if out, _ := corsRulesFromAPI(ctx, nil); out != nil {
		t.Errorf("corsRulesFromAPI(nil) = %v, want nil", out)
	}
}

// TestResolveCORSState covers the computed max_age being resolved before state
// is written. An unknown left in state fails the apply.
func TestResolveCORSState(t *testing.T) {
	t.Parallel()

	m := bucketModel{CORSRules: []corsRuleModel{{MaxAge: types.Int64Unknown()}}}
	sent := []client.CORSRule{{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET"},
	}}
	if diags := resolveCORSState(context.Background(), sent, &m); diags.HasError() {
		t.Fatalf("resolveCORSState: %v", diags)
	}
	if m.CORSRules[0].MaxAge.IsUnknown() {
		t.Error("max_age is still unknown, which would fail the apply")
	}
}
