package zabbix

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"time"

	"github.com/hashicorp/go-cty/cty"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func TestIsLoopback(t *testing.T) {
	for host, want := range map[string]bool{"localhost": true, "127.0.0.1": true, "127.1.2.3": true, "::1": true,
		"127.attacker.example": false, "localhost.evil": false, "zabbix.example.com": false, "10.0.0.1": false} {
		if got := isLoopback(host); got != want {
			t.Errorf("%s: want %v, got %v", host, want, got)
		}
	}
}

func TestProvider_InternalValidate(t *testing.T) {
	if err := Provider().InternalValidate(); err != nil {
		t.Fatal(err)
	}
}

// clearProviderEnv isolates provider tests from ZABBIX_* variables set for acceptance runs.
func clearProviderEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"ZABBIX_URL", "ZABBIX_USERNAME", "ZABBIX_PASSWORD", "ZABBIX_API_TOKEN", "ZABBIX_TLS_INSECURE", "ZABBIX_CA_CERT_FILE"} {
		t.Setenv(k, "")
	}
}

func TestProviderConfigure_AuthValidation(t *testing.T) {
	clearProviderEnv(t)
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "apiinfo.version":
			return "6.4.21", nil
		case "user.login":
			return "tok", nil
		case "user.checkAuthentication":
			if req.Auth != "" {
				t.Error("user.checkAuthentication must be called without an Authorization header")
			}
			var p map[string]string
			_ = json.Unmarshal(req.Params, &p)
			if p["token"] != "t" {
				return nil, &JsonRpcError{Code: -32602, Message: "Invalid params.", Data: "Not authorized."}
			}
			return map[string]string{"userid": "1"}, nil
		}
		return nil, nil
	})
	loopback := strings.Replace(s.URL, "127.0.0.1", "localhost", 1)

	cases := []struct {
		name    string
		raw     map[string]interface{}
		wantErr string
		wantWrn string
	}{
		{"no credentials", map[string]interface{}{"url": s.URL}, "either api_token or both username and password", ""},
		{"password only", map[string]interface{}{"url": s.URL, "password": "x"}, "either api_token", ""},
		{"token ok", map[string]interface{}{"url": s.URL, "api_token": "t"}, "", ""},
		{"token rejected", map[string]interface{}{"url": s.URL, "api_token": "bad"}, "api_token was rejected", ""},
		{"user+pass ok", map[string]interface{}{"url": s.URL, "username": "u", "password": "p"}, "", ""},
		{"bad url", map[string]interface{}{"url": "ftp://x", "api_token": "t"}, "not a valid http(s) URL", ""},
		{"userinfo in url", map[string]interface{}{"url": "https://admin:s3cret-pw@zabbix.example.com/api_jsonrpc.php", "api_token": "t"}, "must not contain user information", ""},
		{"query in url", map[string]interface{}{"url": "https://zabbix.example.com/api_jsonrpc.php?sid=s3cret-pw", "api_token": "t"}, "query string", ""},
		{"tls_insecure with ca_cert_file", map[string]interface{}{"url": s.URL, "api_token": "t", "tls_insecure": true, "ca_cert_file": "ca.pem"}, "mutually exclusive", "TLS certificate verification is disabled"},
		// Early errors must still carry the transport warnings.
		{"no credentials remote http", map[string]interface{}{"url": "http://zabbix.invalid/api_jsonrpc.php"}, "either api_token", "plain HTTP"},
		{"both auth methods explicit", map[string]interface{}{"url": s.URL, "api_token": "t", "username": "u", "password": "p"}, "api_token and username/password are mutually exclusive", ""},
		{"token with stray password", map[string]interface{}{"url": s.URL, "api_token": "t", "password": "p"}, "mutually exclusive", ""},
		{"http loopback no warning", map[string]interface{}{"url": loopback, "api_token": "t"}, "", ""},
		// The plain-HTTP warning must come from providerConfigure itself and
		// must survive a failing configure (DNS error on the .invalid TLD).
		{"remote http warns", map[string]interface{}{"url": "http://zabbix.invalid/api_jsonrpc.php", "api_token": "t"}, "failed to retrieve Zabbix API version", "plain HTTP"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := schema.TestResourceDataRaw(t, Provider().Schema, tc.raw)
			_, diags := providerConfigure(context.Background(), d)
			var errs, warnings []string
			for _, dg := range diags {
				if strings.Contains(dg.Summary+dg.Detail, "s3cret-pw") {
					t.Fatalf("credentials from the URL must never appear in diagnostics: %v", dg)
				}
				if dg.Severity == diag.Error {
					errs = append(errs, dg.Summary)
				} else {
					warnings = append(warnings, dg.Summary)
				}
			}
			if tc.wantWrn != "" && !containsSubstring(warnings, tc.wantWrn) {
				t.Errorf("want warning containing %q, got %v", tc.wantWrn, warnings)
			}
			if tc.wantWrn == "" && len(warnings) != 0 {
				t.Errorf("unexpected warnings: %v", warnings)
			}
			if tc.wantErr != "" {
				if !containsSubstring(errs, tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, diags)
				}
				return
			}
			if diags.HasError() {
				t.Fatalf("unexpected error: %v", diags)
			}
		})
	}
}

// The schema itself must accept env-driven defaults without conflicts:
// validation runs after DefaultFuncs are applied, so any ConflictsWith or
// RequiredWith would fire on values the user never wrote in HCL.
func TestProviderValidate_EnvDefaultsDoNotConflict(t *testing.T) {
	t.Setenv("ZABBIX_USERNAME", "Admin")
	t.Setenv("ZABBIX_PASSWORD", "zabbix")
	if diags := resourceValidate(t, map[string]interface{}{"url": "https://x", "api_token": "t"}); diags.HasError() {
		t.Fatalf("api_token in HCL with credentials in the environment must pass schema validation: %v", diags)
	}
	t.Setenv("ZABBIX_API_TOKEN", "envtok")
	if diags := resourceValidate(t, map[string]interface{}{"url": "https://x", "username": "u", "password": "p"}); diags.HasError() {
		t.Fatalf("credentials in HCL with a token in the environment must pass schema validation: %v", diags)
	}
}

func TestProviderConfigure_CredentialsWinOverEnvToken(t *testing.T) {
	clearProviderEnv(t)
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "apiinfo.version":
			return "6.4.21", nil
		case "user.login":
			return "sess", nil
		}
		t.Errorf("unexpected method %s", req.Method)
		return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
	})
	t.Setenv("ZABBIX_API_TOKEN", "envtok")
	d := schema.TestResourceDataRaw(t, Provider().Schema, map[string]interface{}{"url": s.URL, "username": "u", "password": "p"})
	client, diags := providerConfigure(context.Background(), d)
	if diags.HasError() {
		t.Fatalf("explicit credentials with an env token must configure with a warning, got %v", diags)
	}
	var warned bool
	for _, dg := range diags {
		if strings.Contains(dg.Summary, "Ignoring ZABBIX_API_TOKEN") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("want a warning about the ignored env token, got %v", diags)
	}
	if c := client.(*ZabbixClient); c.apiToken != "" {
		t.Fatal("the env token must be dropped when credentials are explicit")
	}
}

func TestProviderConfigure_TokenWinsOverEnvCredentials(t *testing.T) {
	clearProviderEnv(t)
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "apiinfo.version":
			return "6.4.21", nil
		case "user.checkAuthentication":
			return map[string]string{"userid": "1"}, nil
		}
		t.Errorf("unexpected method %s (user.login must not be called)", req.Method)
		return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
	})
	t.Setenv("ZABBIX_USERNAME", "Admin")
	t.Setenv("ZABBIX_PASSWORD", "zabbix")
	d := schema.TestResourceDataRaw(t, Provider().Schema, map[string]interface{}{"url": s.URL, "api_token": "t"})
	client, diags := providerConfigure(context.Background(), d)
	if diags.HasError() {
		t.Fatalf("api_token with env credentials must configure with a warning, got %v", diags)
	}
	var warned bool
	for _, dg := range diags {
		if strings.Contains(dg.Summary, "Ignoring ZABBIX_USERNAME") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("want a warning about ignored env credentials, got %v", diags)
	}
	if c := client.(*ZabbixClient); c.username != "" || c.password != "" {
		t.Fatal("env credentials must be dropped when api_token is used")
	}
}

func TestProviderConfigure_WarnsOnUntestedVersion(t *testing.T) {
	clearProviderEnv(t)
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		if req.Method == "user.checkAuthentication" {
			return map[string]string{"userid": "1"}, nil
		}
		return "7.0.3", nil
	})
	d := schema.TestResourceDataRaw(t, Provider().Schema, map[string]interface{}{"url": s.URL, "api_token": "t"})
	_, diags := providerConfigure(context.Background(), d)
	if diags.HasError() {
		t.Fatal(diags)
	}
	var summaries []string
	for _, dg := range diags {
		summaries = append(summaries, dg.Summary)
	}
	if !contains(summaries, "Untested Zabbix version") {
		t.Errorf("want version warning, got %v", summaries)
	}
}

func TestPlainHTTPWarning(t *testing.T) {
	if w := plainHTTPWarning("http://zabbix.example.com/api_jsonrpc.php"); len(w) != 1 || w[0].Summary != "Zabbix API is accessed over plain HTTP" {
		t.Errorf("remote http must warn, got %v", w)
	}
	for _, u := range []string{"https://zabbix.example.com/api_jsonrpc.php", "http://localhost:8082/api_jsonrpc.php", "http://127.0.0.1/api_jsonrpc.php"} {
		if w := plainHTTPWarning(u); len(w) != 0 {
			t.Errorf("%s must not warn, got %v", u, w)
		}
	}
	if w := versionDiagnostics("6.4.21"); len(w) != 0 {
		t.Errorf("6.4.x must not warn, got %v", w)
	}
	if w := versionDiagnostics("6.4.1"); len(w) != 0 {
		t.Errorf("6.4.1 is the minimum supported version, got %v", w)
	}
	if w := versionDiagnostics("6.0.30"); len(w) != 1 || w.HasError() {
		t.Errorf("6.0.x must warn, not error, got %v", w)
	}
	if w := versionDiagnostics("6.4.0"); !w.HasError() {
		t.Errorf("6.4.0 must be rejected, got %v", w)
	}
	if w := versionDiagnostics("6.4.0rc1"); len(w) != 1 || w.HasError() {
		t.Errorf("an unparsable 6.4 patch level must warn as untested, never silently pass, got %v", w)
	}
	if w := versionDiagnostics("6.4"); len(w) != 1 || w.HasError() {
		t.Errorf("a bare 6.4 must warn as untested, got %v", w)
	}
}

func TestEnvBoolDefault(t *testing.T) {
	t.Setenv("ZABBIX_TLS_INSECURE", "")
	if v, err := envBoolDefault("ZABBIX_TLS_INSECURE")(); err != nil || v != false {
		t.Errorf("unset: want false, got %v %v", v, err)
	}
	for _, s := range []string{"1", "true", "TRUE"} {
		t.Setenv("ZABBIX_TLS_INSECURE", s)
		if v, err := envBoolDefault("ZABBIX_TLS_INSECURE")(); err != nil || v != true {
			t.Errorf("%q: want true, got %v %v", s, v, err)
		}
	}
	t.Setenv("ZABBIX_TLS_INSECURE", "yes")
	if _, err := envBoolDefault("ZABBIX_TLS_INSECURE")(); err == nil {
		t.Error("invalid boolean must fail")
	}

	// Explicit HCL value wins over the environment.
	t.Setenv("ZABBIX_TLS_INSECURE", "true")
	d := schema.TestResourceDataRaw(t, Provider().Schema, map[string]interface{}{"url": "https://x", "api_token": "t", "tls_insecure": false})
	if d.Get("tls_insecure").(bool) {
		t.Error("tls_insecure = false in config must override ZABBIX_TLS_INSECURE=true")
	}

	// ca_cert_file alone must not conflict with the tls_insecure default, but
	// an environment-driven tls_insecure must not bypass the conflict either.
	t.Setenv("ZABBIX_TLS_INSECURE", "")
	if diags := resourceValidate(t, map[string]interface{}{"url": "https://x", "api_token": "t", "ca_cert_file": "ca.pem"}); diags.HasError() {
		t.Errorf("ca_cert_file alone must be accepted by validation: %v", diags)
	}
	t.Setenv("ZABBIX_TLS_INSECURE", "true")
	d = schema.TestResourceDataRaw(t, Provider().Schema, map[string]interface{}{"url": "https://x", "api_token": "t", "ca_cert_file": "ca.pem"})
	_, diags := providerConfigure(context.Background(), d)
	if !diags.HasError() || !diagContains(diags, diag.Error, "mutually exclusive") {
		t.Errorf("ZABBIX_TLS_INSECURE=true with ca_cert_file must be rejected, got %v", diags)
	}
}

func resourceValidate(t *testing.T, raw map[string]interface{}) diag.Diagnostics {
	t.Helper()
	return Provider().Validate(terraform.NewResourceConfigRaw(raw))
}

func containsSubstring(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func diagContains(diags diag.Diagnostics, sev diag.Severity, sub string) bool {
	for _, dg := range diags {
		if dg.Severity == sev && strings.Contains(dg.Summary+dg.Detail, sub) {
			return true
		}
	}
	return false
}

func TestProviderConfigure_ConfigureErrorPaths(t *testing.T) {
	clearProviderEnv(t)
	versionDown := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		return nil, &JsonRpcError{Code: -32500, Message: "Application error.", Data: "database down"}
	})
	d := schema.TestResourceDataRaw(t, Provider().Schema, map[string]interface{}{"url": versionDown.URL, "api_token": "t", "tls_insecure": true})
	_, diags := providerConfigure(context.Background(), d)
	if !diags.HasError() || !diagContains(diags, diag.Error, "failed to retrieve Zabbix API version") {
		t.Fatalf("apiinfo.version failure must fail configure, got %v", diags)
	}
	if !diagContains(diags, diag.Warning, "TLS certificate verification is disabled") {
		t.Fatalf("warnings must accompany the error, got %v", diags)
	}

	badLogin := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		if req.Method == "apiinfo.version" {
			return "6.4.21", nil
		}
		return nil, &JsonRpcError{Code: -32602, Message: "Invalid params.", Data: "Incorrect user name or password or account is temporarily blocked."}
	})
	d = schema.TestResourceDataRaw(t, Provider().Schema, map[string]interface{}{"url": badLogin.URL, "username": "u", "password": "wrong"})
	_, diags = providerConfigure(context.Background(), d)
	if !diags.HasError() || !diagContains(diags, diag.Error, "failed to authenticate") {
		t.Fatalf("user.login failure must fail configure, got %v", diags)
	}
}

func TestProviderConfigure_RejectsZabbix640(t *testing.T) {
	clearProviderEnv(t)
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		if req.Method != "apiinfo.version" {
			t.Errorf("no call beyond apiinfo.version expected on a rejected version, got %s", req.Method)
		}
		return "6.4.0", nil
	})
	d := schema.TestResourceDataRaw(t, Provider().Schema, map[string]interface{}{"url": s.URL, "api_token": "t"})
	_, diags := providerConfigure(context.Background(), d)
	if !diags.HasError() || !diagContains(diags, diag.Error, "not supported") {
		t.Fatalf("6.4.0 must be rejected with a clear diagnostic, got %v", diags)
	}
}

func TestProviderConfigure_IncompleteCredentialsWithEnvToken(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("ZABBIX_API_TOKEN", "envtok")
	d := schema.TestResourceDataRaw(t, Provider().Schema, map[string]interface{}{"url": "https://zabbix.example.com/api_jsonrpc.php", "username": "u"})
	_, diags := providerConfigure(context.Background(), d)
	if !diags.HasError() || !diagContains(diags, diag.Error, "both username and password") {
		t.Fatalf("username without password must not silently drop the env token and log in with an empty password, got %v", diags)
	}
}

func TestProviderConfigure_ExplicitPasswordConflictsWithExplicitToken(t *testing.T) {
	// api_token and password in HCL, username from the environment: the
	// explicit password must not be silently discarded in favour of the token.
	clearProviderEnv(t)
	t.Setenv("ZABBIX_USERNAME", "Admin")
	d := schema.TestResourceDataRaw(t, Provider().Schema, map[string]interface{}{"url": "https://zabbix.example.com/api_jsonrpc.php", "api_token": "t", "password": "p"})
	_, diags := providerConfigure(context.Background(), d)
	if !diags.HasError() || !diagContains(diags, diag.Error, "mutually exclusive") {
		t.Fatalf("an explicit password next to an explicit token must be a conflict, got %v", diags)
	}
}

func TestProviderConfigure_BothAmbientMethodsConflict(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("ZABBIX_API_TOKEN", "envtok")
	t.Setenv("ZABBIX_USERNAME", "Admin")
	t.Setenv("ZABBIX_PASSWORD", "zabbix")
	d := schema.TestResourceDataRaw(t, Provider().Schema, map[string]interface{}{"url": "https://zabbix.example.com/api_jsonrpc.php"})
	_, diags := providerConfigure(context.Background(), d)
	if !diags.HasError() || !diagContains(diags, diag.Error, "mutually exclusive") {
		t.Fatalf("two ambient auth methods must be a hard error, got %v", diags)
	}
}

func TestForeignMediaTypeFields_ResolvedType(t *testing.T) {
	raw := cty.ObjectVal(map[string]cty.Value{"script": cty.StringVal("return 1;")})
	if err := foreignMediaTypeFields(raw, mediaTypeEmail); err == nil || !strings.Contains(err.Error(), "script is not supported") {
		t.Fatalf("a webhook script next to a type that resolved to email must fail, got %v", err)
	}
	if err := foreignMediaTypeFields(raw, mediaTypeWebhook); err != nil {
		t.Fatalf("script on a webhook is fine, got %v", err)
	}
}

func TestWrittenInRaw(t *testing.T) {
	raw := cty.ObjectVal(map[string]cty.Value{
		"api_token": cty.StringVal("t"),
		"username":  cty.NullVal(cty.String),
	})
	if !writtenInRaw(raw, "api_token") || writtenInRaw(raw, "username") {
		t.Error("raw config must distinguish written attributes from env-injected defaults")
	}
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// planDiff runs the resource's validation and CustomizeDiff on a fresh config.
func planDiff(t *testing.T, r *schema.Resource, raw map[string]interface{}) error {
	t.Helper()
	cfg := terraform.NewResourceConfigRaw(raw)
	if diags := r.Validate(cfg); diags.HasError() {
		return errors.New(diags[0].Summary)
	}
	_, err := r.Diff(context.Background(), nil, cfg, nil)
	return err
}

// unknown is the marker the SDK uses for values not known at plan time.
const unknown = unknownMarker

func TestCustomizeDiff_UnknownValuesAreDeferred(t *testing.T) {
	if err := planDiff(t, resourceHost(), map[string]interface{}{"host": "h", "groups": []interface{}{"2"}, "ip": unknown}); err != nil {
		t.Errorf("host with unknown ip must plan: %v", err)
	}
	if err := planDiff(t, resourceHost(), map[string]interface{}{"host": "h", "groups": []interface{}{unknown}, "use_ip": false, "dns": unknown}); err != nil {
		t.Errorf("host with unknown dns must plan: %v", err)
	}
	if err := planDiff(t, resourceMediaType(), map[string]interface{}{"name": "m", "type": 4, "script": unknown, "timeout": unknown}); err != nil {
		t.Errorf("webhook with unknown script must plan: %v", err)
	}
	if err := planDiff(t, resourceMediaType(), map[string]interface{}{"name": "m", "type": 0, "smtp_server": unknown, "smtp_helo": "h", "smtp_email": "e"}); err != nil {
		t.Errorf("email with unknown smtp_server must plan: %v", err)
	}
	if err := planDiff(t, resourceAction(), map[string]interface{}{"name": "a",
		"condition": []interface{}{map[string]interface{}{"conditiontype": 0, "value": unknown}},
		"operation": []interface{}{map[string]interface{}{"mediatypeid": unknown, "user_groups": unknown}}}); err != nil {
		t.Errorf("action with unknown references must plan: %v", err)
	}
	// Unknown values inside set elements arrive as the SDK marker string, not
	// as typed values: this must not panic and must not produce false errors.
	if err := planDiff(t, resourceAction(), map[string]interface{}{"name": "a",
		"operation": []interface{}{map[string]interface{}{"users": []interface{}{"1"}}},
		"condition": []interface{}{
			map[string]interface{}{"conditiontype": unknown, "value": "1"},
			map[string]interface{}{"conditiontype": 26, "value": "prod", "value2": unknown},
		}}); err != nil {
		t.Errorf("action with unknown values inside condition elements must plan: %v", err)
	}
}

func TestHostCustomizeDiff_NameFollowsHostUnlessConfigured(t *testing.T) {
	r := resourceHost()
	state := &terraform.InstanceState{ID: "1", Attributes: map[string]string{
		"host": "web01", "name": "Renamed in the UI", "enabled": "true", "description": "",
		"groups.#": "1", "groups.0": "2", "use_ip": "true", "ip": "192.0.2.1", "dns": "", "port": "10050",
	}}
	// CustomizeDiff inspects the raw configuration (name present or not); the
	// test harness exposes it through the state.
	rawWithoutName := cty.ObjectVal(map[string]cty.Value{
		"host": cty.StringVal("web01"), "name": cty.NullVal(cty.String),
		"groups": cty.SetVal([]cty.Value{cty.StringVal("2")}), "ip": cty.StringVal("192.0.2.1"),
	})
	state.RawConfig = rawWithoutName
	cfg := terraform.NewResourceConfigRaw(map[string]interface{}{"host": "web01", "groups": []interface{}{"2"}, "ip": "192.0.2.1"})
	diff, err := r.Diff(context.Background(), state, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if diff == nil || diff.Attributes["name"] == nil || diff.Attributes["name"].New != "web01" {
		t.Fatalf("a visible name changed outside Terraform must show up as a diff back to host, got %v", diff)
	}

	state.RawConfig = cty.ObjectVal(map[string]cty.Value{
		"host": cty.StringVal("web01"), "name": cty.StringVal("Renamed in the UI"),
		"groups": cty.SetVal([]cty.Value{cty.StringVal("2")}), "ip": cty.StringVal("192.0.2.1"),
	})
	explicit := terraform.NewResourceConfigRaw(map[string]interface{}{"host": "web01", "name": "Renamed in the UI", "groups": []interface{}{"2"}, "ip": "192.0.2.1"})
	diff, err = r.Diff(context.Background(), state, explicit, nil)
	if err != nil {
		t.Fatal(err)
	}
	if diff != nil && diff.Attributes["name"] != nil {
		t.Fatalf("an explicitly configured name must not be normalised, got %v", diff.Attributes["name"])
	}
}

func TestHostCustomizeDiff_ImportedHostWithoutAgentInterface(t *testing.T) {
	r := resourceHost()
	// State of an imported SNMP-only host: no ip, use_ip defaults to true. The
	// configuration must describe the agent interface to be created.
	state := &terraform.InstanceState{ID: "1", Attributes: map[string]string{
		"host": "snmp-only", "name": "snmp-only", "enabled": "true", "description": "old",
		"groups.#": "1", "groups.0": "2", "use_ip": "true", "ip": "", "dns": "", "port": "10050",
	}}
	withIP := map[string]interface{}{"host": "snmp-only", "groups": []interface{}{"2"}, "description": "new", "ip": "192.0.2.5"}
	diff, err := r.Diff(context.Background(), state, terraform.NewResourceConfigRaw(withIP), nil)
	if err != nil || diff == nil || diff.Attributes["ip"] == nil || diff.Attributes["ip"].New != "192.0.2.5" {
		t.Errorf("configuring the agent interface of an imported host must plan its creation, got diff=%v err=%v", diff, err)
	}
	// No interface attributes configured at all: valid (agentless host).
	noIP := map[string]interface{}{"host": "snmp-only", "groups": []interface{}{"2"}, "description": "new"}
	if _, err := r.Diff(context.Background(), state, terraform.NewResourceConfigRaw(noIP), nil); err != nil {
		t.Errorf("a host without interface attributes must plan (agentless), got %v", err)
	}
}

func TestResourceTimeoutsDefaults(t *testing.T) {
	for name, r := range Provider().ResourcesMap {
		to := r.Timeouts
		if to == nil {
			t.Fatalf("%s: timeouts not configured", name)
		}
		for op, d := range map[string]*time.Duration{"create": to.Create, "read": to.Read, "update": to.Update, "delete": to.Delete} {
			if d == nil || *d != 2*time.Minute {
				t.Errorf("%s: %s timeout must default to 2 minutes, got %v", name, op, d)
			}
		}
	}
}

func TestHostCustomizeDiff(t *testing.T) {
	r := resourceHost()
	groups := []interface{}{"2"}
	if err := planDiff(t, r, map[string]interface{}{"host": "h", "groups": groups, "ip": "10.0.0.1"}); err != nil {
		t.Errorf("valid ip host: %v", err)
	}
	if err := planDiff(t, r, map[string]interface{}{"host": "h", "groups": groups, "ip": "192.0.2.o1"}); err == nil || !strings.Contains(err.Error(), "not a valid IP") {
		t.Errorf("a malformed IP must fail at plan time, got %v", err)
	}
	if err := planDiff(t, r, map[string]interface{}{"host": "h", "groups": groups, "ip": "{$HOST.IP}"}); err != nil {
		t.Errorf("a user macro must be a valid ip: %v", err)
	}
	if err := planDiff(t, r, map[string]interface{}{"host": "h", "groups": groups}); err != nil {
		t.Errorf("a host without any interface must plan (trapper/dependent items only), got %v", err)
	}
	if err := planDiff(t, r, map[string]interface{}{"host": "h", "groups": groups, "use_ip": false, "dns": "bad name"}); err == nil || !strings.Contains(err.Error(), "not a valid DNS name") {
		t.Errorf("a DNS name with whitespace must fail at plan time, got %v", err)
	}
	if err := planDiff(t, r, map[string]interface{}{"host": "h", "groups": groups, "use_ip": false, "dns": "{$AGENT.DNS}"}); err != nil {
		t.Errorf("a user macro must be a valid dns, got %v", err)
	}
	if err := planDiff(t, r, map[string]interface{}{"host": "h", "groups": groups, "dns": "x.local"}); err == nil || !strings.Contains(err.Error(), "ip is required") {
		t.Errorf("dns with use_ip=true must ask for ip or use_ip=false, got %v", err)
	}
	if err := planDiff(t, r, map[string]interface{}{"host": "h", "groups": groups, "port": "10051"}); err == nil || !strings.Contains(err.Error(), "port requires ip or dns") {
		t.Errorf("a custom port without an address must fail (it would never converge), got %v", err)
	}
	if err := planDiff(t, r, map[string]interface{}{"host": "h", "groups": groups, "use_ip": false}); err == nil || !strings.Contains(err.Error(), "dns is required") {
		t.Errorf("dns mode without dns must fail, got %v", err)
	}
	if err := planDiff(t, r, map[string]interface{}{"host": "h", "groups": groups, "ip": "1.1.1.1", "port": "70000"}); err == nil {
		t.Error("port out of range must fail")
	}
	if err := planDiff(t, r, map[string]interface{}{"host": "h", "groups": []interface{}{}, "ip": "1.1.1.1"}); err == nil {
		t.Error("empty groups must fail")
	}
}

func TestMediaTypeCustomizeDiff(t *testing.T) {
	r := resourceMediaType()
	cases := []struct {
		name string
		raw  map[string]interface{}
		want string
	}{
		{"email missing smtp", map[string]interface{}{"name": "m", "type": 0}, "smtp_server is required"},
		{"email ok", map[string]interface{}{"name": "m", "type": 0, "smtp_server": "s", "smtp_helo": "h", "smtp_email": "e"}, ""},
		{"email password without auth", map[string]interface{}{"name": "m", "type": 0, "smtp_server": "s", "smtp_helo": "h", "smtp_email": "e", "password": "p"}, "smtp_authentication = 1"},
		{"email auth without credentials", map[string]interface{}{"name": "m", "type": 0, "smtp_server": "s", "smtp_helo": "h", "smtp_email": "e", "smtp_authentication": 1}, "username is required"},
		{"email auth with credentials", map[string]interface{}{"name": "m", "type": 0, "smtp_server": "s", "smtp_helo": "h", "smtp_email": "e", "smtp_authentication": 1, "username": "u", "password": "p"}, ""},
		{"script missing exec_path", map[string]interface{}{"name": "m", "type": 1}, "exec_path is required"},
		{"webhook missing script", map[string]interface{}{"name": "m", "type": 4}, "script is required"},
		{"webhook bad timeout", map[string]interface{}{"name": "m", "type": 4, "script": "x", "timeout": "5m"}, "timeout must be between"},
		{"webhook timeout macro", map[string]interface{}{"name": "m", "type": 4, "script": "x", "timeout": "{$TIMEOUT}"}, "timeout must be between"},
		{"webhook with unknown foreign field", map[string]interface{}{"name": "m", "type": 4, "script": "x", "smtp_server": unknown}, "smtp_server is not supported for media type 4"},
		{"email unknown server but invalid helo", map[string]interface{}{"name": "m", "type": 0, "smtp_server": unknown, "smtp_helo": "", "smtp_email": "e"}, "smtp_helo is required"},
		{"parameter on email", map[string]interface{}{"name": "m", "type": 0, "smtp_server": "s", "smtp_helo": "h", "smtp_email": "e",
			"parameter": []interface{}{map[string]interface{}{"name": "a", "value": "b"}}}, "parameter is not supported for media type 0"},
		{"unsupported type", map[string]interface{}{"name": "m", "type": 3}, "expected type to be one of"},
		{"email field on webhook", map[string]interface{}{"name": "m", "type": 4, "script": "x", "smtp_port": 587}, "smtp_port is not supported for media type 4"},
		{"webhook field on script", map[string]interface{}{"name": "m", "type": 1, "exec_path": "x", "timeout": "10s"}, "timeout is not supported for media type 1"},
		{"content_type on webhook", map[string]interface{}{"name": "m", "type": 4, "script": "x", "content_type": 0}, "content_type is not supported for media type 4"},
		{"process_tags on email", map[string]interface{}{"name": "m", "type": 0, "smtp_server": "s", "smtp_helo": "h", "smtp_email": "e", "process_tags": true}, "process_tags is not supported for media type 0"},
		{"email content_type ok", map[string]interface{}{"name": "m", "type": 0, "smtp_server": "s", "smtp_helo": "h", "smtp_email": "e", "content_type": 0}, ""},
		{"sms max_sessions", map[string]interface{}{"name": "m", "type": 2, "gsm_modem": "/dev/ttyS0", "max_sessions": 5}, "max_sessions must be 1"},
		{"bad attempt_interval", map[string]interface{}{"name": "m", "type": 4, "script": "x", "attempt_interval": "2h"}, "attempt_interval must be"},
		{"attempt_interval macro", map[string]interface{}{"name": "m", "type": 4, "script": "x", "attempt_interval": "{$IV}"}, "attempt_interval must be"},
		{"event menu without url", map[string]interface{}{"name": "m", "type": 4, "script": "x", "show_event_menu": true}, "event_menu_url is required"},
		{"event menu url without flag", map[string]interface{}{"name": "m", "type": 4, "script": "x", "event_menu_url": "https://x"}, "requires show_event_menu"},
		{"event menu ok", map[string]interface{}{"name": "m", "type": 4, "script": "x", "show_event_menu": true, "event_menu_url": "https://x/{EVENT.ID}", "event_menu_name": "Open"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := planDiff(t, r, tc.raw)
			if tc.want == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestActionCustomizeDiff(t *testing.T) {
	r := resourceAction()
	op := func(extra map[string]interface{}) []interface{} {
		m := map[string]interface{}{}
		for k, v := range extra {
			m[k] = v
		}
		return []interface{}{m}
	}
	cases := []struct {
		name string
		raw  map[string]interface{}
		want string
	}{
		{"no recipients", map[string]interface{}{"name": "a", "operation": op(nil)}, "at least one recipient"},
		{"users ok", map[string]interface{}{"name": "a", "operation": op(map[string]interface{}{"users": []interface{}{"1"}})}, ""},
		{"steps inverted", map[string]interface{}{"name": "a", "operation": op(map[string]interface{}{"user_groups": []interface{}{"7"}, "esc_step_from": 3, "esc_step_to": 2})}, "esc_step_to"},
		{"esc_period too short", map[string]interface{}{"name": "a", "esc_period": "30s", "operation": op(map[string]interface{}{"users": []interface{}{"1"}})}, "between 60 seconds and 1 week"},
		{"esc_period zero not allowed on action", map[string]interface{}{"name": "a", "esc_period": "0", "operation": op(map[string]interface{}{"users": []interface{}{"1"}})}, "between 60 seconds and 1 week"},
		{"esc_period macro ok", map[string]interface{}{"name": "a", "esc_period": "{$ESC}", "operation": op(map[string]interface{}{"users": []interface{}{"1"}})}, ""},
		{"operation esc_period zero ok", map[string]interface{}{"name": "a", "operation": op(map[string]interface{}{"users": []interface{}{"1"}, "esc_period": "0"})}, ""},
		{"no operations", map[string]interface{}{"name": "a"}, "Missing required argument"},
		{"evaltype custom unsupported", map[string]interface{}{"name": "a", "evaltype": 3, "operation": op(map[string]interface{}{"users": []interface{}{"1"}})}, "expected evaltype"},
		{"eventsource unsupported", map[string]interface{}{"name": "a", "eventsource": 1, "operation": op(map[string]interface{}{"users": []interface{}{"1"}})}, "expected eventsource"},
		{"value2 without tag type", map[string]interface{}{"name": "a", "operation": op(map[string]interface{}{"users": []interface{}{"1"}}), "condition": []interface{}{map[string]interface{}{"conditiontype": 0, "value": "1", "value2": "x"}}}, "only supported for condition type 26"},
		{"tag type without value2", map[string]interface{}{"name": "a", "operation": op(map[string]interface{}{"users": []interface{}{"1"}}), "condition": []interface{}{map[string]interface{}{"conditiontype": 26, "value": "prod"}}}, "requires value2"},
		{"tag type ok", map[string]interface{}{"name": "a", "operation": op(map[string]interface{}{"users": []interface{}{"1"}}), "condition": []interface{}{map[string]interface{}{"conditiontype": 26, "operator": 2, "value": "prod", "value2": "env"}}}, ""},
		{"event name with equals operator", map[string]interface{}{"name": "a", "operation": op(map[string]interface{}{"users": []interface{}{"1"}}), "condition": []interface{}{map[string]interface{}{"conditiontype": 3, "value": "disk"}}}, "operator 0 is not valid for condition type 3"},
		{"empty condition value", map[string]interface{}{"name": "a", "operation": op(map[string]interface{}{"users": []interface{}{"1"}}), "condition": []interface{}{map[string]interface{}{"conditiontype": 0, "value": "   "}}}, "must not be empty"},
		{"severity out of range", map[string]interface{}{"name": "a", "operation": op(map[string]interface{}{"users": []interface{}{"1"}}), "condition": []interface{}{map[string]interface{}{"conditiontype": 4, "operator": 5, "value": "9"}}}, "value 0-5"},
		{"time period with in operator", map[string]interface{}{"name": "a", "operation": op(map[string]interface{}{"users": []interface{}{"1"}}), "condition": []interface{}{map[string]interface{}{"conditiontype": 6, "operator": 4, "value": "1-7,00:00-24:00"}}}, ""},
		{"severity with contains operator", map[string]interface{}{"name": "a", "operation": op(map[string]interface{}{"users": []interface{}{"1"}}), "condition": []interface{}{map[string]interface{}{"conditiontype": 4, "operator": 2, "value": "4"}}}, "operator 2 is not valid for condition type 4"},
		{"removed condition type 16", map[string]interface{}{"name": "a", "operation": op(map[string]interface{}{"users": []interface{}{"1"}}), "condition": []interface{}{map[string]interface{}{"conditiontype": 16, "operator": 10, "value": ""}}}, "expected condition.0.conditiontype to be one of"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := planDiff(t, r, tc.raw)
			if tc.want == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestReadError(t *testing.T) {
	d := schema.TestResourceDataRaw(t, resourceHostGroup().Schema, map[string]interface{}{"name": "g"})
	d.SetId("42")

	if diags := readError(context.Background(), d, "host group", errors.New("timeout")); !diags.HasError() || d.Id() != "42" {
		t.Errorf("transport error must be surfaced and keep the ID, got diags=%v id=%q", diags, d.Id())
	}
	diags := readError(context.Background(), d, "host group", ErrNotFound)
	if diags.HasError() || d.Id() != "" || len(diags) != 1 || diags[0].Severity != diag.Warning {
		t.Errorf("not found must clear the ID with a warning, got diags=%v id=%q", diags, d.Id())
	}
}

func TestDeleteError(t *testing.T) {
	missing := &JsonRpcError{Code: -32500, Message: "Application error.", Data: objectMissing}
	confirmGone := func(context.Context) error { return ErrNotFound }
	confirmPresent := func(context.Context) error { return nil }

	if err := deleteError(context.Background(), missing, confirmGone); err != nil {
		t.Errorf("confirmed absence must be success, got %v", err)
	}
	if err := deleteError(context.Background(), missing, confirmPresent); err == nil {
		t.Error("object still visible (permission problem) must stay an error")
	}
	if err := deleteError(context.Background(), errors.New("boom"), confirmGone); err == nil {
		t.Error("other errors must be returned")
	}
}

func TestSuppressEquivalentDuration(t *testing.T) {
	cases := []struct {
		old, new string
		want     bool
	}{
		{"1h", "3600", true}, {"3600s", "1h", true}, {"0", "0s", true},
		{"1h", "3601", false}, {"{$X}", "{$X}", false}, {"1h", "{$X}", false}, {"", "1h", false},
	}
	for _, tc := range cases {
		if got := suppressEquivalentDuration("k", tc.old, tc.new, nil); got != tc.want {
			t.Errorf("%q vs %q: want %v, got %v", tc.old, tc.new, tc.want, got)
		}
	}
}

func TestParseZabbixDuration(t *testing.T) {
	cases := map[string]int{"0": 0, "90": 90, "30s": 30, "5m": 300, "1h": 3600, "1d": 86400, "1w": 604800, "{$MACRO}": -1}
	for in, want := range cases {
		got, err := parseZabbixDuration(in)
		if err != nil || got != want {
			t.Errorf("%q: want %d, got %d (%v)", in, want, got, err)
		}
	}
	for _, bad := range []string{"", "1x", "abc", "-5", "1h30m", "99999999999999999999", "4611686018427387905m", "9999999999w"} {
		if _, err := parseZabbixDuration(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestStringsDiff(t *testing.T) {
	got := stringsDiff([]string{"a", "b", "c"}, []string{"b"})
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("want [a c], got %v", got)
	}
}
