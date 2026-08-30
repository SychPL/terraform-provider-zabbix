package zabbix

import (
	"context"
	"fmt"
	"net/url"

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
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("ZABBIX_USERNAME", nil),
				Description: "Zabbix API username",
			},
			"password": {
				Type:        schema.TypeString,
				Optional:    true,
				Sensitive:   true,
				DefaultFunc: schema.EnvDefaultFunc("ZABBIX_PASSWORD", nil),
				Description: "Zabbix API password",
			},
			"api_token": {
				Type:        schema.TypeString,
				Optional:    true,
				Sensitive:   true,
				DefaultFunc: schema.EnvDefaultFunc("ZABBIX_API_TOKEN", nil),
				Description: "Zabbix API Token (Bearer)",
			},
			"tls_insecure": {
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     false,
				DefaultFunc: schema.EnvDefaultFunc("ZABBIX_TLS_INSECURE", nil),
				Description: "Disable SSL verification for HTTPS Zabbix endpoint",
			},
			"ca_cert_file": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("ZABBIX_CA_CERT_FILE", nil),
				Description: "Path to CA certificate file for SSL verification",
			},
		},
		ResourcesMap: map[string]*schema.Resource{
			"zabbix_host_group": resourceHostGroup(),
			"zabbix_host":       resourceHost(),
			"zabbix_media_type": resourceMediaType(),
			"zabbix_action":     resourceAction(),
		},
		ConfigureContextFunc: providerConfigure,
	}
}

func providerConfigure(ctx context.Context, d *schema.ResourceData) (interface{}, diag.Diagnostics) {
	urlStr := d.Get("url").(string)
	username := d.Get("username").(string)
	password := d.Get("password").(string)
	apiToken := d.Get("api_token").(string)
	tlsInsecure := d.Get("tls_insecure").(bool)
	caCertFile := d.Get("ca_cert_file").(string)

	if apiToken == "" && (username == "" || password == "") {
		return nil, diag.Errorf("either api_token or both username and password must be configured")
	}

	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, diag.Errorf("invalid url: %s", err)
	}
	if u.User != nil {
		return nil, diag.Errorf("url must not contain credentials (username/password)")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, diag.Errorf("url must not contain a query string or fragment")
	}
	if tlsInsecure && caCertFile != "" {
		return nil, diag.Errorf("tls_insecure and ca_cert_file are mutually exclusive")
	}

	client, err := NewZabbixClient(urlStr, username, password, apiToken, tlsInsecure, caCertFile)
	if err != nil {
		return nil, diag.Errorf("failed to initialize Zabbix client: %s", err)
	}

	var diags diag.Diagnostics

	// Authenticate client (only if username/password are used)
	if apiToken == "" {
		if err := client.Login(ctx); err != nil {
			return nil, diag.Errorf("failed to authenticate with Zabbix API: %s", err)
		}
	}

	// Retrieve Zabbix API version
	version, err := client.GetVersion(ctx)
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
