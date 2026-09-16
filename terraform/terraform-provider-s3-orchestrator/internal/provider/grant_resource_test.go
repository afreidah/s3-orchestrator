// -------------------------------------------------------------------------------
// Acceptance Tests - Grant
//
// Author: Alex Freidah
//
// The grant is the resource with the most to get wrong, and each of the three
// tests here covers one of them. Narrowing a permission set has to be an update
// rather than a replacement, or the apply leaves a window where the client
// reaches nothing. A grant naming no kind has to stay on bucket across replans
// rather than going unknown and tripping its own replacement. And the composite
// identifier has to survive an import, because a grant has no id of its own.
// -------------------------------------------------------------------------------

package provider

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// TestAccGrantBucket covers a bucket grant through narrowing and import.
func TestAccGrantBucket(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckGrantDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccGrantConfig(`["list", "read", "write"]`),
				Check: resource.ComposeAggregateTestCheckFunc(
					// Naming no kind lands on bucket, which is what lets the
					// common case name only the bucket.
					resource.TestCheckResourceAttr("s3orchestrator_grant.test", "kind", "bucket"),
					resource.TestCheckResourceAttr("s3orchestrator_grant.test", "name", testBucket),
					resource.TestCheckResourceAttr("s3orchestrator_grant.test", "permissions.#", "3"),
					testAccCheckGrantPermissions("s3orchestrator_grant.test", "list", "read", "write"),
				),
			},
			{
				// A replan against an unchanged configuration must be empty:
				// the defaulted kind going unknown here is what replaced a
				// grant nobody touched before the plan modifier was added.
				Config:   testAccGrantConfig(`["list", "read", "write"]`),
				PlanOnly: true,
			},
			{
				ResourceName:      "s3orchestrator_grant.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				Config: testAccGrantConfig(`["read"]`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						// Narrowing replaces the set in place. A replacement
						// would revoke first and leave the client with nothing
						// in between, which is what the upsert exists to avoid.
						plancheck.ExpectResourceAction(
							"s3orchestrator_grant.test", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("s3orchestrator_grant.test", "permissions.#", "1"),
					testAccCheckGrantPermissions("s3orchestrator_grant.test", "read"),
				),
			},
		},
	})
}

// TestAccGrantOrchestrator covers the kind that takes no resource name, whose
// identifier therefore ends in an empty segment.
func TestAccGrantOrchestrator(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckGrantDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccGrantOrchestratorConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(
						"s3orchestrator_grant.test", "kind", "orchestrator"),
					resource.TestCheckNoResourceAttr("s3orchestrator_grant.test", "name"),
					resource.TestMatchResourceAttr(
						"s3orchestrator_grant.test", "id", regexpOrchestratorID),
					testAccCheckGrantPermissions("s3orchestrator_grant.test", "admin-read"),
				),
			},
			{
				ResourceName:      "s3orchestrator_grant.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccGrantWrongVocabulary covers the orchestrator refusing bucket
// permissions on an orchestrator grant. The two vocabularies do not mix, and
// the error belongs to the deployment rather than to the provider.
func TestAccGrantWrongVocabulary(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      testAccGrantWrongVocabularyConfig,
				ExpectError: regexpNotValidOn,
			},
		},
	})
}

// -------------------------------------------------------------------------
// FIXTURES
// -------------------------------------------------------------------------

// regexpOrchestratorID matches an identifier whose third segment is empty,
// which is how a grant on the orchestrator keys.
var regexpOrchestratorID = regexp.MustCompile(`/orchestrator/$`)

// regexpNotValidOn matches the orchestrator's refusal of a permission the kind
// does not take. Whitespace is loose because Terraform rewraps a diagnostic to
// the terminal width, which puts line breaks inside the sentence.
var regexpNotValidOn = regexp.MustCompile(`not\s+valid\s+on`)

// testAccGrantConfig is one bucket grant carrying the permissions given,
// naming no kind so the default is exercised on every step.
func testAccGrantConfig(permissions string) string {
	return fmt.Sprintf(`
provider "s3orchestrator" {}

resource "s3orchestrator_user" "test" {
  name = "acc-grant-bucket"
}

resource "s3orchestrator_grant" "test" {
  user_id     = s3orchestrator_user.test.id
  name        = %q
  permissions = %s
}
`, testBucket, permissions)
}

// testAccGrantOrchestratorConfig grants over the deployment itself, which takes
// no resource name.
const testAccGrantOrchestratorConfig = `
provider "s3orchestrator" {}

resource "s3orchestrator_user" "test" {
  name = "acc-grant-orchestrator"
}

resource "s3orchestrator_grant" "test" {
  user_id     = s3orchestrator_user.test.id
  kind        = "orchestrator"
  permissions = ["admin-read"]
}
`

// testAccGrantWrongVocabularyConfig asks for a data-plane permission on the
// orchestrator.
const testAccGrantWrongVocabularyConfig = `
provider "s3orchestrator" {}

resource "s3orchestrator_user" "test" {
  name = "acc-grant-vocabulary"
}

resource "s3orchestrator_grant" "test" {
  user_id     = s3orchestrator_user.test.id
  kind        = "orchestrator"
  permissions = ["read"]
}
`

// -------------------------------------------------------------------------
// CHECKS
// -------------------------------------------------------------------------

// testAccCheckGrantPermissions asks the orchestrator what the grant carries,
// which is the only way to tell a written permission set from one Terraform
// merely recorded.
func testAccCheckGrantPermissions(addr string, want ...string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[addr]
		if !ok {
			return fmt.Errorf("%s not in state", addr)
		}
		grant, found, err := testAccClient().Grant(context.Background(),
			rs.Primary.Attributes["user_id"],
			rs.Primary.Attributes["kind"],
			rs.Primary.Attributes["name"])
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("grant %q in state but not in the orchestrator", rs.Primary.ID)
		}
		if !sameSet(grant.Permissions, want) {
			return fmt.Errorf("grant %q carries %v, want %v", rs.Primary.ID, grant.Permissions, want)
		}
		return nil
	}
}

// testAccCheckGrantDestroy confirms the withdrawal reached the orchestrator.
//
// A grant is nested under its user, so one whose user is already gone reads as
// absent, which is the answer this wants either way.
func testAccCheckGrantDestroy(s *terraform.State) error {
	api := testAccClient()
	for _, rs := range s.RootModule().Resources {
		if rs.Type != "s3orchestrator_grant" {
			continue
		}
		_, found, err := api.Grant(context.Background(),
			rs.Primary.Attributes["user_id"],
			rs.Primary.Attributes["kind"],
			rs.Primary.Attributes["name"])
		if err != nil {
			return err
		}
		if found {
			return fmt.Errorf("grant %q survived destroy", rs.Primary.ID)
		}
	}
	return nil
}
