package zabbix

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

// SupportedVersionPrefix is the Zabbix API version line the provider is tested against.
const SupportedVersionPrefix = "6.4"

func Provider() *schema.Provider {
	return &schema.Provider{
		Schema: map[string]*schema.Schema{
			"url": {
				Type:        schema.TypeString,
				Required:    true,
				DefaultFunc: schema.EnvDefaultFunc("ZABBIX_URL", nil),
				Description: "Zabbix API JSON-RPC endpoint URL (e.g. https://zabbix.example.com/api_jsonrpc.php). Can also be set with the `ZABBIX_URL` environment variable.",
			},
			"username": {
				Type:          schema.TypeString,
				Optional:      true,
				DefaultFunc:   schema.EnvDefaultFunc("ZABBIX_USERNAME", nil),
				ConflictsWith: []string{"api_token"},
				RequiredWith:  []string{"password"},
				Description:   "Zabbix API username. Required together with `password` unless `api_token` is set. Can also be set with `ZABBIX_USERNAME`.",
			},
			"password": {
				Type:          schema.TypeString,
				Optional:      true,
				Sensitive:     true,
				DefaultFunc:   schema.EnvDefaultFunc("ZABBIX_PASSWORD", nil),
				ConflictsWith: []string{"api_token"},
				RequiredWith:  []string{"username"},
				Description:   "Zabbix API password. Can also be set with `ZABBIX_PASSWORD`.",
			},
			"api_token": {
				Type:        schema.TypeString,
				Optional:    true,
				Sensitive:   true,
				DefaultFunc: schema.EnvDefaultFunc("ZABBIX_API_TOKEN", nil),
				Description: "Zabbix API token (Administration -> API tokens). Alternative to `username`/`password`. Can also be set with `ZABBIX_API_TOKEN`.",
			},
			"tls_insecure": {
				Type:          schema.TypeBool,
				Optional:      true,
				Default:       false,
				ConflictsWith: []string{"ca_cert_file"},
				Description:   "Skip TLS certificate verification. Only for testing. Can also be set with `ZABBIX_TLS_INSECURE`.",
			},
			"ca_cert_file": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("ZABBIX_CA_CERT_FILE", nil),
				Description: "Path to a PEM file with CA certificates used to verify the Zabbix server certificate. Can also be set with `ZABBIX_CA_CERT_FILE`.",
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
	cfg := ClientConfig{
		URL:        d.Get("url").(string),
		Username:   d.Get("username").(string),
		Password:   d.Get("password").(string),
		APIToken:   d.Get("api_token").(string),
		Insecure:   d.Get("tls_insecure").(bool) || strings.EqualFold(envOr("ZABBIX_TLS_INSECURE", "false"), "true"),
		CACertFile: d.Get("ca_cert_file").(string),
	}

	var diags diag.Diagnostics

	if cfg.APIToken == "" && (cfg.Username == "" || cfg.Password == "") {
		return nil, diag.Errorf("either api_token or both username and password must be configured (or the ZABBIX_API_TOKEN / ZABBIX_USERNAME / ZABBIX_PASSWORD environment variables)")
	}
	if cfg.APIToken != "" && cfg.Username != "" {
		return nil, diag.Errorf("api_token and username/password are mutually exclusive")
	}

	if u, err := url.Parse(cfg.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, diag.Errorf("url %q is not a valid http(s) URL", cfg.URL)
	} else if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		diags = append(diags, diag.Diagnostic{
			Severity: diag.Warning,
			Summary:  "Zabbix API is accessed over plain HTTP",
			Detail:   fmt.Sprintf("Credentials and API tokens are sent in clear text to %s. Use https:// for anything but local testing.", cfg.URL),
		})
	}

	client, err := NewZabbixClient(cfg)
	if err != nil {
		return nil, diag.FromErr(err)
	}

	version, err := client.GetVersion(ctx)
	if err != nil {
		return nil, diag.Errorf("failed to retrieve Zabbix API version from %s: %s", cfg.URL, err)
	}
	if !strings.HasPrefix(version, SupportedVersionPrefix+".") {
		diags = append(diags, diag.Diagnostic{
			Severity: diag.Warning,
			Summary:  "Untested Zabbix version",
			Detail:   fmt.Sprintf("This provider is tested against Zabbix %s.x; the server reports %s. Some features may behave unexpectedly.", SupportedVersionPrefix, version),
		})
	}

	if err := client.Login(ctx); err != nil {
		return nil, diag.Errorf("failed to authenticate with Zabbix API: %s", err)
	}

	return client, diags
}

func isLoopback(host string) bool {
	return host == "localhost" || strings.HasPrefix(host, "127.") || host == "::1"
}
