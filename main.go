// terraform-provider-mgcl exposes mgcl bootstrap secrets to OpenTofu. The
// mgcl_secret ephemeral resource keeps every KV out of plan and state; the
// provider::mgcl::secret_value function reads public KVs and returns ordinary
// data that OpenTofu may persist; the mgcl_secret_kv managed resource writes
// one KV and always stores its value in plan and state.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/geektr-cloud/terraform-provider-mgcl/internal/provider"
)

// version is overridden at build time with
// -ldflags "-X main.version=0.2.0".
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
