// Terraform provider for k0rdent AI.
//
// The registry address this binary serves is derived from the provider type
// name, which is not settled — see internal/provider/names.go. Nothing is
// published until it is.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// version is overridden at release time via -ldflags.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run the provider with support for debuggers like delve")
	flag.Parse()

	opts := providerserver.ServeOpts{
		// RegistryAddress is the only other place the provider type name
		// appears in Go, and it is derived rather than written out.
		Address: provider.RegistryAddress(),
		Debug:   debug,
	}

	if err := providerserver.Serve(context.Background(), provider.New(version), opts); err != nil {
		log.Fatal(err.Error())
	}
}
