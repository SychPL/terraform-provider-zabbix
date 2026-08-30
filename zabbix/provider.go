package zabbix

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/go-cty/cty"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

// SupportedVersionPrefix is the Zabbix API version line the provider is tested against.
const SupportedVersionPrefix = "6.4"

// configureTimeout bounds the version check and authentication performed
// while configuring the provider.
const configureTimeout = 2 * time.Minute

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
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("ZABBIX_USERNAME", nil),
				Description: "Zabbix API username. Required together with `password` unless `api_token` is set. Can also be set with `ZABBIX_USERNAME`.",
			},
			"password": {
				Type:        schema.TypeString,
				Optional:    true,
				Sensitive:   true,
				DefaultFunc: schema.EnvDefaultFunc("ZABBIX_PASSWORD", nil),
				Description: "Zabbix API password. Can also be set with `ZABBIX_PASSWORD`.",
			},
			"api_token": {
				Type:        schema.TypeString,
				Optional:    true,
				Sensitive:   true,
				DefaultFunc: schema.EnvDefaultFunc("ZABBIX_API_TOKEN", nil),
				Description: "Zabbix API token (Administration -> API tokens). Alternative to `username`/`password`. Can also be set with `ZABBIX_API_TOKEN`.",
			},
			"tls_insecure": {
				Type:        schema.TypeBool,
				Optional:    true,
				DefaultFunc: envBoolDefault("ZABBIX_TLS_INSECURE"),
				Description: "Skip TLS certificate verification. Only for testing. Conflicts with `ca_cert_file`. Can also be set with `ZABBIX_TLS_INSECURE`.",
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
		Insecure:   d.Get("tls_insecure").(bool),
		CACertFile: d.Get("ca_cert_file").(string),
	}

	var diags diag.Diagnostics

	// URL validation and the transport warnings come first so that every later
	// error (missing credentials, TLS conflicts, failed probes) still carries
	// e.g. the plain-HTTP warning about secrets sent in clear text.
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, diag.Errorf("url is not a valid http(s) URL")
	}
	if u.User != nil {
		return nil, diag.Errorf("url must not contain user information; use username/password or api_token instead")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, diag.Errorf("url must not contain a query string or fragment")
	}
	diags = append(diags, plainHTTPWarning(cfg.URL)...)
	if cfg.Insecure {
		diags = append(diags, diag.Diagnostic{
			Severity: diag.Warning,
			Summary:  "TLS certificate verification is disabled",
			Detail:   "tls_insecure is enabled (possibly via ZABBIX_TLS_INSECURE); the server identity is not verified.",
		})
	}

	if cfg.APIToken == "" && (cfg.Username == "" || cfg.Password == "") {
		return nil, append(diags, diag.Errorf("either api_token or both username and password must be configured (or the ZABBIX_API_TOKEN / ZABBIX_USERNAME / ZABBIX_PASSWORD environment variables)")...)
	}
	if cfg.APIToken != "" && (cfg.Username != "" || cfg.Password != "") {
		// Both methods resolved (typically one of them from globally exported
		// ZABBIX_* variables in CI): the explicitly configured one wins; two
		// explicit methods (or two ambient ones) are a hard error.
		tokenExplicit := attrWrittenInConfig(d, "api_token", "ZABBIX_API_TOKEN")
		credsExplicit := attrWrittenInConfig(d, "username", "ZABBIX_USERNAME") ||
			attrWrittenInConfig(d, "password", "ZABBIX_PASSWORD")
		switch {
		case tokenExplicit && !credsExplicit:
			diags = append(diags, diag.Diagnostic{
				Severity: diag.Warning,
				Summary:  "Ignoring ZABBIX_USERNAME/ZABBIX_PASSWORD",
				Detail:   "api_token is configured; the username/password from the environment are not used.",
			})
			cfg.Username, cfg.Password = "", ""
		case credsExplicit && !tokenExplicit:
			diags = append(diags, diag.Diagnostic{
				Severity: diag.Warning,
				Summary:  "Ignoring ZABBIX_API_TOKEN",
				Detail:   "username/password are configured; the API token from the environment is not used.",
			})
			cfg.APIToken = ""
		default:
			return nil, append(diags, diag.Errorf("api_token and username/password are mutually exclusive")...)
		}
		// Re-validate after dropping the ambient method: the remaining explicit
		// credentials may be incomplete (e.g. username in HCL without password
		// must not silently fall back to a login with an empty password).
		if cfg.APIToken == "" && (cfg.Username == "" || cfg.Password == "") {
			return nil, append(diags, diag.Errorf("both username and password must be configured (the API token from the environment is ignored for explicitly configured credentials)")...)
		}
	}
	// Checked here, not with ConflictsWith: both attributes have environment
	// defaults, and a default must neither trigger a false conflict nor let
	// tls_insecure silently disable the verification a CA file asks for.
	if cfg.Insecure && cfg.CACertFile != "" {
		return nil, append(diags, diag.Errorf("tls_insecure and ca_cert_file are mutually exclusive")...)
	}

	client, err := NewZabbixClient(cfg)
	if err != nil {
		return nil, append(diags, diag.FromErr(err)...)
	}

	// Resource operations get their deadline from the resource timeouts; the
	// configuration probes need their own.
	ctx, cancel := context.WithTimeout(ctx, configureTimeout)
	defer cancel()

	// Errors below are appended to the collected warnings so that the operator
	// still sees e.g. the plain-HTTP warning when configure fails.
	version, err := client.GetVersion(ctx)
	if err != nil {
		return nil, append(diags, diag.Errorf("failed to retrieve Zabbix API version from %s: %s", cfg.URL, err)...)
	}
	diags = append(diags, versionDiagnostics(version)...)
	if diags.HasError() {
		return nil, diags
	}

	if err := client.Login(ctx); err != nil {
		return nil, append(diags, diag.Errorf("failed to authenticate with Zabbix API: %s", err)...)
	}
	if cfg.APIToken != "" {
		// Sessions are validated by Login; tokens only by an authenticated call.
		if err := client.CheckAuth(ctx); err != nil {
			return nil, append(diags, diag.Errorf("api_token was rejected by the Zabbix API: %s", err)...)
		}
	}

	return client, diags
}

// attrWrittenInConfig reports whether the attribute was written in the
// configuration, as opposed to injected by its environment DefaultFunc. The
// raw config is authoritative and always available under a real Terraform
// core; the env-comparison fallback exists only for the schema test harness
// (which cannot carry a raw config) and misclassifies a config value that
// happens to equal the environment variable - acceptable in tests, never hit
// in production.
func attrWrittenInConfig(d *schema.ResourceData, attr, envVar string) bool {
	if raw := d.GetRawConfig(); !raw.IsNull() {
		return writtenInRaw(raw, attr)
	}
	return os.Getenv(envVar) != d.Get(attr).(string)
}

func writtenInRaw(raw cty.Value, attr string) bool {
	return !raw.GetAttr(attr).IsNull()
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// plainHTTPWarning warns when credentials would be sent in clear text.
func plainHTTPWarning(rawURL string) diag.Diagnostics {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" || isLoopback(u.Hostname()) {
		return nil
	}
	return diag.Diagnostics{{
		Severity: diag.Warning,
		Summary:  "Zabbix API is accessed over plain HTTP",
		Detail:   fmt.Sprintf("Credentials and API tokens are sent in clear text to %s. Use https:// for anything but local testing.", rawURL),
	}}
}

// versionDiagnostics validates the reported server version. 6.4.0 is rejected:
// the token parameter of user.checkAuthentication, which api_token validation
// relies on, only exists since 6.4.1 (ZBXNEXT-8012). Other version lines are
// untested and produce a warning.
func versionDiagnostics(version string) diag.Diagnostics {
	if rest, ok := strings.CutPrefix(version, SupportedVersionPrefix+"."); ok {
		// Only a plain numeric patch level counts as a tested release; anything
		// else (release candidates, betas) falls through to the untested
		// warning instead of silently passing the gate (fail-closed).
		if patch, err := strconv.Atoi(rest); err == nil {
			if patch < 1 {
				return diag.Errorf("Zabbix %s is not supported; the minimum supported release is %s.1 (6.4.0 cannot validate API tokens - ZBXNEXT-8012)", version, SupportedVersionPrefix)
			}
			return nil
		}
	}
	return diag.Diagnostics{{
		Severity: diag.Warning,
		Summary:  "Untested Zabbix version",
		Detail:   fmt.Sprintf("This provider is tested against Zabbix %s.x; the server reports %s. Some features may behave unexpectedly.", SupportedVersionPrefix, version),
	}}
}

// envBoolDefault reads a boolean default from the environment; HCL always wins.
func envBoolDefault(key string) schema.SchemaDefaultFunc {
	return func() (interface{}, error) {
		v := os.Getenv(key)
		if v == "" {
			return false, nil
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		return b, nil
	}
}
