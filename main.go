package main

import (
	"github.com/Tensai123/terraform-provider-zabbix/zabbix"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/plugin"
)

// version is injected by goreleaser via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	zabbix.Version = version
	plugin.Serve(&plugin.ServeOpts{
		ProviderFunc: func() *schema.Provider {
			return zabbix.Provider()
		},
	})
}
