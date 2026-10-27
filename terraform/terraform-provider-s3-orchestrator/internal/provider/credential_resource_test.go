// -------------------------------------------------------------------------------
// Acceptance Tests - Credential
//
// Author: Alex Freidah
//
// Covers both halves of the keypair story against a running orchestrator: one
// minted by the orchestrator and one supplied by the caller. The supplied path
// is the one a secret manager composes with, so it is checked to land exactly
// the keypair it was given rather than a minted one.
//
// An import cannot carry the secret, because nothing reads one back out. The
// import step therefore ignores it, which is the behaviour rather than a gap in
// the test.
// -------------------------------------------------------------------------------

package provider

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// regexpHalfKeypair matches the validator's summary. It is matched rather than
// compared because Terraform wraps a diagnostic in its own framing.
var regexpHalfKeypair = regexp.MustCompile(`Half a keypair`)

// The keypair the supplied-keypair case registers. It never signs anything;
// what is under test is that both halves survive the round trip.
const (
	testAccSuppliedKey    = "AKIASUPPLIEDACCTEST0"
	testAccSuppliedSecret = "supplied-acceptance-secret"
)

// TestAccCredentialMinted covers a credential the orchestrator generates.
func TestAccCredentialMinted(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckDestroyed("s3orchestrator_credential", testAccCredentialFound),
		Steps: []resource.TestStep{
			{
				Config: testAccCredentialMintedConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(
						"s3orchestrator_credential.test", "access_key_id"),
					resource.TestCheckResourceAttrSet(
						"s3orchestrator_credential.test", "secret_access_key"),
					resource.TestCheckResourceAttr(
						"s3orchestrator_credential.test", "label", "minted by terraform"),
					// The id is the access key, which is what every later
					// lookup and the import identifier are.
					resource.TestCheckResourceAttrPair(
						"s3orchestrator_credential.test", "id",
						"s3orchestrator_credential.test", "access_key_id"),
					testAccCheckExists("s3orchestrator_credential.test", testAccCredentialFound),
				),
			},
			{
				ResourceName:      "s3orchestrator_credential.test",
				ImportState:       true,
				ImportStateVerify: true,
				// The orchestrator never hands a secret back, so an imported
				// credential has none until the configuration supplies it.
				ImportStateVerifyIgnore: []string{"secret_access_key"},
			},
		},
	})
}

// TestAccCredentialSupplied covers a keypair the caller already holds.
func TestAccCredentialSupplied(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckDestroyed("s3orchestrator_credential", testAccCredentialFound),
		Steps: []resource.TestStep{
			{
				Config: testAccCredentialSuppliedConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("s3orchestrator_credential.test",
						"access_key_id", testAccSuppliedKey),
					resource.TestCheckResourceAttr("s3orchestrator_credential.test",
						"secret_access_key", testAccSuppliedSecret),
					testAccCheckExists("s3orchestrator_credential.test", testAccCredentialFound),
				),
			},
		},
	})
}

// TestAccCredentialHalfKeypair covers the validator, which reports a typo at
// plan time rather than letting the orchestrator refuse it during an apply.
func TestAccCredentialHalfKeypair(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      testAccCredentialHalfKeypairConfig,
				ExpectError: regexpHalfKeypair,
				PlanOnly:    true,
			},
		},
	})
}

// -------------------------------------------------------------------------
// FIXTURES
// -------------------------------------------------------------------------

// testAccCredentialMintedConfig names no keypair, so the orchestrator makes one.
const testAccCredentialMintedConfig = `
provider "s3orchestrator" {}

resource "s3orchestrator_user" "test" {
  name = "acc-credential-minted"
}

resource "s3orchestrator_credential" "test" {
  user_id = s3orchestrator_user.test.id
  label   = "minted by terraform"
}
`

// testAccCredentialSuppliedConfig registers a keypair generated elsewhere.
const testAccCredentialSuppliedConfig = `
provider "s3orchestrator" {}

resource "s3orchestrator_user" "test" {
  name = "acc-credential-supplied"
}

resource "s3orchestrator_credential" "test" {
  user_id           = s3orchestrator_user.test.id
  access_key_id     = "` + testAccSuppliedKey + `"
  secret_access_key = "` + testAccSuppliedSecret + `"
}
`

// testAccCredentialHalfKeypairConfig names a key with no secret.
const testAccCredentialHalfKeypairConfig = `
provider "s3orchestrator" {}

resource "s3orchestrator_user" "test" {
  name = "acc-credential-half"
}

resource "s3orchestrator_credential" "test" {
  user_id       = s3orchestrator_user.test.id
  access_key_id = "AKIAHALFKEYPAIRTEST0"
}
`

// -------------------------------------------------------------------------
// CHECKS
// -------------------------------------------------------------------------

// testAccCredentialFound looks a keypair up by its access key, and fails one
// that proves a different user than state says.
func testAccCredentialFound(is *terraform.InstanceState) (bool, error) {
	cred, found, err := testAccClient().Credential(context.Background(), is.ID)
	if found && cred.UserID != is.Attributes["user_id"] {
		return true, fmt.Errorf("credential %q proves user %q, state says %q",
			is.ID, cred.UserID, is.Attributes["user_id"])
	}
	return found, err
}
