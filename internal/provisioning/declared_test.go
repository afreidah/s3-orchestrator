// -------------------------------------------------------------------------------
// Provisioning - Declared Bucket Set Tests
//
// Author: Alex Freidah
//
// Covers what the live set reports before its first store, after one, and
// after a swap replaces it, and the per-bucket settings it answers: a bucket's
// multipart limit, the stored form its new objects take, its replication
// factor, and the scope of a pass adding a layer to existing copies.
// -------------------------------------------------------------------------------

package provisioning

import (
	"slices"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestDeclared_ContainsFollowsTheStoredSet verifies Contains reports nothing
// before a store and from a nil holder, and the names of whichever set was
// stored last.
func TestDeclared_ContainsFollowsTheStoredSet(t *testing.T) {
	t.Parallel()
	var nilSet *Declared
	d := NewDeclared()
	if nilSet.Contains("photos") || d.Contains("photos") || d.Buckets() != nil {
		t.Fatal("an empty holder reported a bucket")
	}

	d.Store([]Bucket{{Name: "photos"}, {Name: "logs"}})
	if !d.Contains("photos") || !d.Contains("logs") || d.Contains("other") {
		t.Errorf("Contains does not match the stored set %v", d.Names())
	}
	if !d.HasPrefix("photos/a.jpg") || d.HasPrefix("photos") {
		t.Error("HasPrefix does not match the stored set")
	}

	d.Store([]Bucket{{Name: "other"}})
	if d.Contains("photos") || !d.Contains("other") || !slices.Equal(d.Names(), []string{"other"}) {
		t.Errorf("a swap did not replace the set: %v", d.Names())
	}
}

// TestDeclared_MaxMultipartUploads verifies a bucket's upload limit is read
// from the set, and that a bucket with none, or one not declared, has none.
func TestDeclared_MaxMultipartUploads(t *testing.T) {
	t.Parallel()
	d := NewDeclared()
	d.Store([]Bucket{{Name: "limited", MaxMultipartUploads: 50}, {Name: "unlimited"}})
	for bucket, want := range map[string]int{"limited": 50, "unlimited": 0, "nonexistent": 0} {
		if got := d.MaxMultipartUploads(bucket); got != want {
			t.Errorf("MaxMultipartUploads(%q) = %d, want %d", bucket, got, want)
		}
	}
}

// TestDeclared_Form verifies a bucket with a stored form of its own takes
// exactly its layers, even none, and every other bucket, declared or not,
// takes the deployment's as it stands when asked.
func TestDeclared_Form(t *testing.T) {
	t.Parallel()
	deployment := core.StoredCompressed | core.StoredEncrypted
	d := NewDeclared()
	if got := d.Form("photos"); got != 0 {
		t.Errorf("Form before a deployment form is named = %v, want none", got)
	}
	live := config.NewLive(&config.Config{Compression: config.CompressionConfig{Enabled: true}})
	d.UseDeployment(live, true)
	d.Store([]Bucket{
		{Name: "restic", StoredForm: &[]string{config.LayerHashed}},
		{Name: "traces", StoredForm: &[]string{}},
		{Name: "photos"},
	})
	for bucket, want := range map[string]core.StoredFlags{
		"restic": core.StoredHashed, "traces": 0, "photos": deployment, "nonexistent": deployment,
	} {
		if got := d.Form(bucket); got != want {
			t.Errorf("Form(%q) = %v, want %v", bucket, got, want)
		}
	}
	live.Update(func(c *config.Config) { c.Compression.Enabled, c.Integrity.Enabled = false, true })
	if got := d.Form("photos"); got != core.StoredEncrypted|core.StoredHashed {
		t.Errorf("Form after the deployment form changed = %v, want encrypted,hashed", got)
	}
}

// TestDeclared_Factors verifies the factors name the deployment's as it
// stands when asked, and only the buckets setting one of their own.
func TestDeclared_Factors(t *testing.T) {
	t.Parallel()
	d := NewDeclared()
	if f := d.Factors(); f.Replicates() {
		t.Errorf("Factors before a deployment is named = %v, want none", f.String())
	}
	live := config.NewLive(&config.Config{Replication: config.ReplicationConfig{Factor: 2}})
	d.UseDeployment(live, false)
	d.Store([]Bucket{{Name: "traces", ReplicationFactor: 1}, {Name: "photos"}})
	f := d.Factors()
	if got := f.String(); got != "2,traces=1" {
		t.Errorf("Factors = %q, want 2,traces=1", got)
	}
	if f.For("traces/a") != 1 || f.For("photos/a") != 2 {
		t.Errorf("For gave traces %d and photos %d, want 1 and 2", f.For("traces/a"), f.For("photos/a"))
	}
	live.Update(func(c *config.Config) { c.Replication.Factor = 3 })
	if got := d.Factors(); got.Default != 3 {
		t.Errorf("Default after the deployment factor changed = %d, want 3", got.Default)
	}
}

// TestDeclared_Wanting verifies a layer the deployment applies scopes a pass to
// every bucket but those leaving it out, and one it does not apply scopes it to
// only the buckets naming it.
func TestDeclared_Wanting(t *testing.T) {
	t.Parallel()
	d := NewDeclared()
	d.UseDeployment(config.NewLive(&config.Config{}), true)
	d.Store([]Bucket{
		{Name: "restic", StoredForm: &[]string{config.LayerHashed}},
		{Name: "vault", StoredForm: &[]string{config.LayerEncrypted, config.LayerHashed}},
		{Name: "photos"},
	})
	cases := []struct {
		layer core.StoredFlags
		want  core.BucketScope
	}{
		{core.StoredEncrypted, core.BucketScope{Names: []string{"restic"}}},
		{core.StoredHashed, core.BucketScope{Names: []string{"restic", "vault"}, Only: true}},
		{core.StoredCompressed, core.BucketScope{Only: true}},
	}
	for _, tc := range cases {
		got := d.Wanting(tc.layer)
		if got.Only != tc.want.Only || !slices.Equal(got.Names, tc.want.Names) {
			t.Errorf("Wanting(%v) = %+v, want %+v", tc.layer, got, tc.want)
		}
	}
}

// TestDeploymentForm verifies each section a deployment enables gives the
// layer it applies, encryption follows the encryptor rather than config, and
// a set told no deployment stores no layer.
func TestDeploymentForm(t *testing.T) {
	t.Parallel()
	d := NewDeclared()
	if got := d.DeploymentForm(); got != 0 {
		t.Errorf("DeploymentForm(no deployment) = %v, want none", got)
	}
	cfg := &config.Config{}
	d.UseDeployment(config.NewLive(cfg), false)
	if got := d.DeploymentForm(); got != 0 {
		t.Errorf("DeploymentForm(nothing enabled) = %v, want none", got)
	}
	cfg.Compression.Enabled, cfg.Integrity.Enabled, cfg.Encryption.Enabled = true, true, true
	d.UseDeployment(config.NewLive(cfg), false)
	if got := d.DeploymentForm(); got != core.StoredCompressed|core.StoredHashed {
		t.Errorf("DeploymentForm(no encryptor) = %v, want compressed,hashed", got)
	}
	d.UseDeployment(config.NewLive(cfg), true)
	if got := d.DeploymentForm(); got != core.StoredCompressed|core.StoredEncrypted|core.StoredHashed {
		t.Errorf("DeploymentForm(encryptor) = %v, want every layer", got)
	}
}
