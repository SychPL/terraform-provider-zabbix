package zabbix

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

func Provider() *schema.Provider {
	return &schema.Provider{
		Schema: map[string]*schema.Schema{
			"url": {
				Type:        schema.TypeString,
				Required:    true,
				DefaultFunc: schema.EnvDefaultFunc("ZABBIX_URL", nil),
				Description: "Zabbix API JSON-RPC endpoint URL (e.g. http://localhost/zabbix/api_jsonrpc.php)",
			},
			"username": {
				Type:        schema.TypeString,
				Required:    true,
				DefaultFunc: schema.EnvDefaultFunc("ZABBIX_USERNAME", nil),
				Description: "Zabbix API username",
			},
			"password": {
				Type:        schema.TypeString,
				Required:    true,
				Sensitive:   true,
				DefaultFunc: schema.EnvDefaultFunc("ZABBIX_PASSWORD", nil),
				Description: "Zabbix API password",
			},
		},
		ResourcesMap: map[string]*schema.Resource{
			"zabbix_host_group": resourceHostGroup(),
			"zabbix_host":       resourceHost(),
		},
		ConfigureContextFunc: providerConfigure,
	}
}

func providerConfigure(ctx context.Context, d *schema.ResourceData) (interface{}, diag.Diagnostics) {
	url := d.Get("url").(string)
	username := d.Get("username").(string)
	password := d.Get("password").(string)

	client := NewZabbixClient(url, username, password)

	var diags diag.Diagnostics

	// Authenticate client
	if err := client.Login(); err != nil {
		return nil, diag.Errorf("failed to authenticate with Zabbix API: %s", err)
	}

	// Retrieve Zabbix API version
	version, err := client.GetVersion()
	if err != nil {
		return nil, diag.Errorf("failed to retrieve Zabbix API version: %s", err)
	}

	// Validate Zabbix 6.4 target version compatibility
	if len(version) < 3 || version[:3] != "6.4" {
		diags = append(diags, diag.Diagnostic{
			Severity: diag.Warning,
			Summary:  "Zabbix API version mismatch",
			Detail:   fmt.Sprintf("This provider is optimized for Zabbix API version 6.4.x. Found Zabbix server version: %s. Some features may behave unexpectedly.", version),
		})
	}

	return client, diags
}
