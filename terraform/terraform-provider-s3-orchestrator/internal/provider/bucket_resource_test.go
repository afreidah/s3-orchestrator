// -------------------------------------------------------------------------------
// Acceptance Tests - Bucket
//
// Author: Alex Freidah
//
// Drives the resource against a real orchestrator: create, replan, import, and
// the two updates that matter. Changing a limit has to be an update rather than
// a replacement, because the orchestrator refuses to delete a bucket that holds
// anything, and dropping a rule has to remove it rather than leave it behind.
//
// The bucket this creates is its own. testBucket is config-declared, which is
// what the read-only case covers instead.
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

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestAccBucket covers the resource's whole life: declared, replanned clean,
// imported, widened in place, and finally stripped of its rules.
func TestAccBucket(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckBucketDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccBucketConfig(2, `
  cors_rule {
    allowed_origins = ["https://example.com"]
    allowed_methods = ["GET"]
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("s3orchestrator_bucket.test", "name", "acc-bucket"),
					resource.TestCheckResourceAttr("s3orchestrator_bucket.test", "max_multipart_uploads", "2"),
					resource.TestCheckResourceAttr("s3orchestrator_bucket.test", "cors_rule.#", "1"),
					resource.TestCheckResourceAttr("s3orchestrator_bucket.test",
						"cors_rule.0.allowed_origins.0", "https://example.com"),
				),
			},
			{
				// An unchanged configuration must replan empty. An optional
				// list read back as an empty one rather than as null is the
				// usual way this drifts.
				Config: testAccBucketConfig(2, `
  cors_rule {
    allowed_origins = ["https://example.com"]
    allowed_methods = ["GET"]
  }`),
				PlanOnly: true,
			},
			{
				ResourceName:  "s3orchestrator_bucket.test",
				ImportState:   true,
				ImportStateId: "acc-bucket",
				// The bucket has no id of its own; the name identifies it and
				// every object under it. Without naming it here the harness
				// looks for an "id" attribute the resource does not carry.
				ImportStateVerifyIdentifierAttribute: "name",
				ImportStateVerify:                    true,
			},
			{
				// The limit changes in place. A replacement would destroy the
				// bucket first, which the orchestrator refuses once anything is
				// stored under it.
				Config: testAccBucketConfig(9, `
  cors_rule {
    allowed_origins = ["https://example.com"]
    allowed_methods = ["GET"]
  }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(
							"s3orchestrator_bucket.test", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.TestCheckResourceAttr(
					"s3orchestrator_bucket.test", "max_multipart_uploads", "9"),
			},
			{
				// Dropping the block removes the rule: the update replaces what
				// the bucket carries rather than merging into it.
				Config: testAccBucketConfig(9, ""),
				Check: resource.TestCheckResourceAttr(
					"s3orchestrator_bucket.test", "cors_rule.#", "0"),
			},
		},
	})
}

// TestAccBucketRenameReplaces verifies the name forces replacement, since it
// identifies every object stored beneath it.
func TestAccBucketRenameReplaces(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCheckBucketDestroy,
		Steps: []resource.TestStep{
			{Config: testAccNamedBucketConfig("acc-first")},
			{
				Config: testAccNamedBucketConfig("acc-second"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("s3orchestrator_bucket.test",
							plancheck.ResourceActionDestroyBeforeCreate),
					},
				},
			},
		},
	})
}

// TestAccBucketConfigDeclaredIsReadOnly verifies importing a bucket the config
// file owns reports what it is rather than reading as a permissions failure.
func TestAccBucketConfigDeclaredIsReadOnly(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:        testAccNamedBucketConfig(testBucket),
				ResourceName:  "s3orchestrator_bucket.test",
				ImportState:   true,
				ImportStateId: testBucket,
				ExpectError:   regexp.MustCompile("configuration file"),
			},
		},
	})
}

// TestAccBucketDataSource verifies the data source reads a bucket the config
// file declares, which the resource will not manage.
func TestAccBucketDataSource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
data "s3orchestrator_bucket" "test" {
  name = %q
}`, testBucket),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.s3orchestrator_bucket.test", "name", testBucket),
					resource.TestCheckResourceAttr("data.s3orchestrator_bucket.test", "source", "config"),
				),
			},
		},
	})
}

// TestAccBucketDataSourceMissing verifies a name nothing declares stops the plan
// rather than reporting an empty bucket.
func TestAccBucketDataSourceMissing(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "s3orchestrator_bucket" "test" {
  name = "nothing-declares-this"
}`,
				ExpectError: regexp.MustCompile("Bucket not found"),
			},
		},
	})
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// testAccBucketConfig renders the resource with a limit and whatever blocks the
// case needs.
func testAccBucketConfig(maxUploads int, blocks string) string {
	return fmt.Sprintf(`
resource "s3orchestrator_bucket" "test" {
  name                  = "acc-bucket"
  max_multipart_uploads = %d
%s
}`, maxUploads, blocks)
}

// testAccNamedBucketConfig renders the smallest bucket under a given name.
func testAccNamedBucketConfig(name string) string {
	return fmt.Sprintf(`
resource "s3orchestrator_bucket" "test" {
  name = %q
}`, name)
}

// testAccCheckBucketDestroy verifies every bucket the case declared is gone.
func testAccCheckBucketDestroy(s *terraform.State) error {
	api := testAccClient()
	for _, rs := range s.RootModule().Resources {
		if rs.Type != "s3orchestrator_bucket" {
			continue
		}
		name := rs.Primary.Attributes["name"]
		_, found, err := api.Bucket(context.Background(), name)
		if err != nil {
			return err
		}
		if found {
			return fmt.Errorf("bucket %q survived destroy", name)
		}
	}
	return nil
}
