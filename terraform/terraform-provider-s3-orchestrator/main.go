// -------------------------------------------------------------------------------
// Terraform Provider - Entry Point
//
// Author: Alex Freidah
//
// Serves the provider over gRPC. Terraform launches this binary as a
// subprocess and speaks its plugin protocol to it; nothing here runs on its
// own, and running it by hand prints the handshake and exits.
//
// The address is the provider's registry identity, which is what a consumer
// names in required_providers. It is declared here rather than derived so a
// development override and a published release answer to the same name.
// -------------------------------------------------------------------------------

package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/afreidah/s3-orchestrator/terraform/terraform-provider-s3-orchestrator/internal/provider"
)

// providerAddress is what a consumer writes in required_providers. It has to
// match a development override exactly for Terraform to find this binary.
const providerAddress = "registry.terraform.io/afreidah/s3-orchestrator"

// version is stamped at build time with -ldflags. The default is what a
// locally built binary reports, which is the one a development override runs.
var version = "dev"

// main serves the provider until Terraform closes the connection.
func main() {
	// Debug mode prints a TF_REATTACH_PROVIDERS value and waits, so a debugger
	// can attach to this process and Terraform can be pointed at it instead of
	// spawning its own copy.
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run with support for debuggers like delve")
	flag.Parse()

	if err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: providerAddress,
		Debug:   debug,
	}); err != nil {
		log.Fatal(err)
	}
}
