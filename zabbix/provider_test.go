package zabbix

import (
	"context"
	"errors"
	"strings"
	"testing"

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
		case "user.get":
			if req.Auth != "Bearer t" {
				return nil, &JsonRpcError{Code: -32602, Message: "Invalid params.", Data: sessionTerminated}
			}
			return []map[string]string{{"userid": "1"}}, nil
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
		{"userinfo in url", map[string]interface{}{"url": "https://admin:s3cret@zabbix.example.com/api_jsonrpc.php", "api_token": "t"}, "must not contain user information", ""},
		{"http loopback no warning", map[string]interface{}{"url": loopback, "api_token": "t"}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := schema.TestResourceDataRaw(t, Provider().Schema, tc.raw)
			_, diags := providerConfigure(context.Background(), d)
			if tc.wantErr != "" {
				if !diags.HasError() || !strings.Contains(diags[0].Summary, tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, diags)
				}
				return
			}
			if diags.HasError() {
				t.Fatalf("unexpected error: %v", diags)
			}
			for _, dg := range diags {
				if tc.wantWrn == "" {
					t.Errorf("unexpected warning: %s", dg.Summary)
				}
			}
		})
	}
}

func TestProviderConfigure_WarnsOnUntestedVersion(t *testing.T) {
	clearProviderEnv(t)
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		if req.Method == "user.get" {
			return []map[string]string{{"userid": "1"}}, nil
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
	if w := versionWarning("6.4.21"); len(w) != 0 {
		t.Errorf("6.4.x must not warn, got %v", w)
	}
	if w := versionWarning("6.0.30"); len(w) != 1 {
		t.Errorf("6.0.x must warn")
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
const unknown = "74D93920-ED26-11E3-AC10-0800200C9A66"

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
	// State of an imported SNMP-only host: no ip, use_ip defaults to true.
	state := &terraform.InstanceState{ID: "1", Attributes: map[string]string{
		"host": "snmp-only", "name": "snmp-only", "enabled": "true", "description": "old",
		"groups.#": "1", "groups.0": "2", "use_ip": "true", "ip": "", "dns": "", "port": "10050",
	}}
	base := map[string]interface{}{"host": "snmp-only", "groups": []interface{}{"2"}}

	desc := map[string]interface{}{"description": "new"}
	for k, v := range base {
		desc[k] = v
	}
	if _, err := r.Diff(context.Background(), state, terraform.NewResourceConfigRaw(desc), nil); err != nil {
		t.Errorf("changing description of a host without agent interface must plan: %v", err)
	}

	port := map[string]interface{}{"port": "10051"}
	for k, v := range base {
		port[k] = v
	}
	if _, err := r.Diff(context.Background(), state, terraform.NewResourceConfigRaw(port), nil); err == nil || !strings.Contains(err.Error(), "ip is required") {
		t.Errorf("changing interface attributes without ip must still fail, got %v", err)
	}
}

func TestHostCustomizeDiff(t *testing.T) {
	r := resourceHost()
	groups := []interface{}{"2"}
	if err := planDiff(t, r, map[string]interface{}{"host": "h", "groups": groups, "ip": "10.0.0.1"}); err != nil {
		t.Errorf("valid ip host: %v", err)
	}
	if err := planDiff(t, r, map[string]interface{}{"host": "h", "groups": groups}); err == nil || !strings.Contains(err.Error(), "ip is required") {
		t.Errorf("use_ip without ip must fail, got %v", err)
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

func TestParseZabbixDuration(t *testing.T) {
	cases := map[string]int{"0": 0, "90": 90, "30s": 30, "5m": 300, "1h": 3600, "1d": 86400, "1w": 604800, "{$MACRO}": -1}
	for in, want := range cases {
		got, err := parseZabbixDuration(in)
		if err != nil || got != want {
			t.Errorf("%q: want %d, got %d (%v)", in, want, got, err)
		}
	}
	for _, bad := range []string{"", "1x", "abc", "-5", "1h30m", "99999999999999999999"} {
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
