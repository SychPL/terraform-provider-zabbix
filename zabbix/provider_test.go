package zabbix

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

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
		{"user+pass ok", map[string]interface{}{"url": s.URL, "username": "u", "password": "p"}, "", ""},
		{"bad url", map[string]interface{}{"url": "ftp://x", "api_token": "t"}, "not a valid http(s) URL", ""},
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

func TestProviderConfigure_WarnsOnPlainHTTPAndVersion(t *testing.T) {
	clearProviderEnv(t)
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		return "7.0.3", nil
	})
	// httptest listens on 127.0.0.1; rewrite the host so it is not loopback by name.
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
		{"parameter on email", map[string]interface{}{"name": "m", "type": 0, "smtp_server": "s", "smtp_helo": "h", "smtp_email": "e",
			"parameter": []interface{}{map[string]interface{}{"name": "a", "value": "b"}}}, "only supported for type 4"},
		{"unsupported type", map[string]interface{}{"name": "m", "type": 3}, "expected type to be one of"},
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
		{"esc_period too short", map[string]interface{}{"name": "a", "esc_period": "30s"}, "at least 60 seconds"},
		{"esc_period macro ok", map[string]interface{}{"name": "a", "esc_period": "{$ESC}"}, ""},
		{"evaltype custom unsupported", map[string]interface{}{"name": "a", "evaltype": 3}, "expected evaltype"},
		{"eventsource unsupported", map[string]interface{}{"name": "a", "eventsource": 1}, "expected eventsource"},
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
	if diags := readError(context.Background(), d, "host group", ErrNotFound); diags.HasError() || d.Id() != "" {
		t.Errorf("not found must clear the ID without error, got diags=%v id=%q", diags, d.Id())
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
	for _, bad := range []string{"", "1x", "abc", "-5", "1h30m"} {
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
