package zabbix

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

func TestProviderConfigure_Validation(t *testing.T) {
	cases := []struct {
		name      string
		inputRaw  map[string]interface{}
		expectErr string
	}{
		{
			name: "Missing both auth options",
			inputRaw: map[string]interface{}{
				"url": "http://localhost/zabbix/api_jsonrpc.php",
			},
			expectErr: "either api_token or both username and password must be configured",
		},
		{
			name: "Inline credentials in URL",
			inputRaw: map[string]interface{}{
				"url":       "http://admin:zabbix@localhost/zabbix/api_jsonrpc.php",
				"api_token": "token123",
			},
			expectErr: "url must not contain credentials (username/password)",
		},
		{
			name: "Query string in URL",
			inputRaw: map[string]interface{}{
				"url":       "http://localhost/zabbix/api_jsonrpc.php?sid=secret",
				"api_token": "token123",
			},
			expectErr: "url must not contain a query string or fragment",
		},
		{
			name: "Fragment in URL",
			inputRaw: map[string]interface{}{
				"url":       "http://localhost/zabbix/api_jsonrpc.php#section",
				"api_token": "token123",
			},
			expectErr: "url must not contain a query string or fragment",
		},
		{
			name: "TLS Insecure and CA Cert conflict",
			inputRaw: map[string]interface{}{
				"url":          "http://localhost/zabbix/api_jsonrpc.php",
				"api_token":    "token123",
				"tls_insecure": true,
				"ca_cert_file": "/path/to/ca.pem",
			},
			expectErr: "tls_insecure and ca_cert_file are mutually exclusive",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Provider()
			d := schema.TestResourceDataRaw(t, p.Schema, tc.inputRaw)

			_, diags := p.ConfigureContextFunc(context.Background(), d)
			if !diags.HasError() {
				t.Fatalf("expected configuration error, got none")
			}

			errMessage := diags[0].Summary
			if !strings.Contains(errMessage, tc.expectErr) {
				t.Fatalf("expected error message containing %q, got %q", tc.expectErr, errMessage)
			}
		})
	}
}
