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

func TestHostCreateUpdate_NoInterface(t *testing.T) {
	var createdWithInterface bool
	var interfaceDeleted bool

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)

		w.Header().Set("Content-Type", "application/json")
		switch payload.Method {
		case "user.login":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":"token_xyz","id":1}`))
		case "host.create":
			var hostParams struct {
				Interfaces []interface{} `json:"interfaces"`
			}
			_ = json.Unmarshal(payload.Params, &hostParams)
			if len(hostParams.Interfaces) > 0 {
				createdWithInterface = true
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"hostids":["10"]},"id":1}`))
		case "host.get":
			// Return a host with 1 agent interface first
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":[{"hostid":"10","host":"test-host","interfaces":[{"interfaceid":"5","type":"1","main":"1","useip":"1","ip":"192.0.2.1","dns":"","port":"10050"}]}],"id":1}`))
		case "hostinterface.get":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":[{"interfaceid":"5","type":"1","main":"1","useip":"1","ip":"192.0.2.1","dns":"","port":"10050"}],"id":1}`))
		case "hostinterface.delete":
			interfaceDeleted = true
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"interfaceids":["5"]},"id":1}`))
		case "host.update":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"hostids":["10"]},"id":1}`))
		default:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{},"id":1}`))
		}
	}))
	defer s.Close()

	// 1. Create a host WITHOUT an interface (ip and dns are empty)
	r := resourceHost()
	dCreate := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{
		"host":   "test-host",
		"groups": []interface{}{"2"},
	})

	c, _ := NewZabbixClient(s.URL, "admin", "zabbix", "", false, "")
	diags := r.CreateContext(context.Background(), dCreate, c)
	if diags.HasError() {
		t.Fatalf("failed to create host: %v", diags)
	}

	if createdWithInterface {
		t.Fatalf("expected host to be created without an interface, but interfaces array was sent")
	}

	// 2. Perform an update that deletes the interface (setting IP and DNS to empty)
	dUpdate := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{
		"host":   "test-host",
		"groups": []interface{}{"2"},
		"ip":     "",
		"dns":    "",
	})
	dUpdate.SetId("10")

	diags = r.UpdateContext(context.Background(), dUpdate, c)
	if diags.HasError() {
		t.Fatalf("failed to update host: %v", diags)
	}

	if !interfaceDeleted {
		t.Fatalf("expected host interface to be deleted, but hostinterface.delete was not called")
	}
}
