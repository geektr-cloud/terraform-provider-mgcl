// terraform-provider-mgcl exposes mgcl bootstrap secrets to OpenTofu as an
// ephemeral resource so values never enter plan or state.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/geektr-cloud/terraform-provider-mgcl/internal/provider"
)

// version is overridden at build time with
// -ldflags "-X main.version=0.1.0".
var version = "dev"

func main() {
	var debug bool

	flag.BoolVar(&debug, "debug", false, "run the provider with debugger support")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.opentofu.org/geektr-cloud/mgcl",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err.Error())
	}
}
