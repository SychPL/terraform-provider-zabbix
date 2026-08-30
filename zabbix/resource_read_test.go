package zabbix

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

func TestMediaTypeCreate_Validation(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)

		w.Header().Set("Content-Type", "application/json")
		switch payload.Method {
		case "user.login":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":"token_xyz","id":1}`))
		case "mediatype.create":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"mediatypeids":["10"]},"id":1}`))
		case "mediatype.get":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":[{"mediatypeid":"10","name":"test","type":"0","status":"0","smtp_server":"localhost"}],"id":1}`))
		default:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{},"id":1}`))
		}
	}))
	defer s.Close()

	cases := []struct {
		name      string
		inputRaw  map[string]interface{}
		expectErr bool
	}{
		{
			name: "Valid Email",
			inputRaw: map[string]interface{}{
				"name":        "email",
				"type":        0,
				"smtp_server": "localhost",
			},
			expectErr: false,
		},
		{
			name: "Invalid Email (no smtp_server)",
			inputRaw: map[string]interface{}{
				"name": "email",
				"type": 0,
			},
			expectErr: true,
		},
		{
			name: "Valid Script",
			inputRaw: map[string]interface{}{
				"name":      "script",
				"type":      1,
				"exec_path": "notify.sh",
			},
			expectErr: false,
		},
		{
			name: "Invalid Script (no exec_path)",
			inputRaw: map[string]interface{}{
				"name": "script",
				"type": 1,
			},
			expectErr: true,
		},
		{
			name: "Valid Webhook",
			inputRaw: map[string]interface{}{
				"name":   "webhook",
				"type":   4,
				"script": "return 'OK';",
			},
			expectErr: false,
		},
		{
			name: "Invalid Webhook (no script)",
			inputRaw: map[string]interface{}{
				"name": "webhook",
				"type": 4,
			},
			expectErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := resourceMediaType()
			d := schema.TestResourceDataRaw(t, r.Schema, tc.inputRaw)

			c, _ := NewZabbixClient(s.URL, "admin", "zabbix", "", false, "")
			diags := r.CreateContext(context.Background(), d, c)

			hasErr := diags.HasError()
			if hasErr != tc.expectErr {
				t.Fatalf("expected error: %t, got error: %t: %v", tc.expectErr, hasErr, diags)
			}
		})
	}
}
