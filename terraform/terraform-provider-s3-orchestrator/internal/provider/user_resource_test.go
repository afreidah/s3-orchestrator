// -------------------------------------------------------------------------------
// Acceptance Tests - User
//
// Author: Alex Freidah
//
// Drives a real apply against a running orchestrator. What matters here beyond
// the resource existing is that a rename is an update rather than a
// replacement: the id a user's credentials and grants reference survives one,
// and a provider that replaced the user instead would revoke every keypair
// proving it.
// -------------------------------------------------------------------------------

package provider

import (
	"context"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// TestAccUser covers the resource's whole lifecycle in one state: create,
// import, rename in place, and destroy.
func TestAccUser(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckDestroyed("s3orchestrator_user", testAccUserFound),
		Steps: []resource.TestStep{
			{
				Config: testAccUserConfig("acc-user"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("s3orchestrator_user.test", "name", "acc-user"),
					resource.TestCheckResourceAttrSet("s3orchestrator_user.test", "id"),
					testAccCheckExists("s3orchestrator_user.test", testAccUserFound),
				),
			},
			{
				ResourceName:      "s3orchestrator_user.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				Config: testAccUserConfig("acc-user-renamed"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						// The whole point of the rename path: an update, not a
						// destroy-and-create that would take the credentials with it.
						plancheck.ExpectResourceAction(
							"s3orchestrator_user.test", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.TestCheckResourceAttr(
					"s3orchestrator_user.test", "name", "acc-user-renamed"),
			},
		},
	})
}

// -------------------------------------------------------------------------
// FIXTURES
// -------------------------------------------------------------------------

// testAccUserConfig is one user under the name given. The provider block is
// empty because its address and keypair arrive through the environment, which
// is the only way to name a port chosen when the container started.
func testAccUserConfig(name string) string {
	return fmt.Sprintf(`
provider "s3orchestrator" {}

resource "s3orchestrator_user" "test" {
  name = %q
}
`, name)
}

// -------------------------------------------------------------------------
// CHECKS
// -------------------------------------------------------------------------

// testAccUserFound looks a user up by its id, so a provider that wrote state
// without writing the user is caught.
func testAccUserFound(is *terraform.InstanceState) (bool, error) {
	_, found, err := testAccClient().User(context.Background(), is.ID)
	return found, err
}
