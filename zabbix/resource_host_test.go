package zabbix

import (
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

func TestGetStringList(t *testing.T) {
	resourceSchema := map[string]*schema.Schema{
		"list_attr": {
			Type:     schema.TypeList,
			Optional: true,
			Elem:     &schema.Schema{Type: schema.TypeString},
		},
		"set_attr": {
			Type:     schema.TypeSet,
			Optional: true,
			Elem:     &schema.Schema{Type: schema.TypeString},
		},
	}

	tests := []struct {
		name     string
		inputRaw map[string]interface{}
		key      string
		expected []string
	}{
		{
			name: "List with string values",
			inputRaw: map[string]interface{}{
				"list_attr": []interface{}{"val1", "val2"},
			},
			key:      "list_attr",
			expected: []string{"val1", "val2"},
		},
		{
			name: "Set with string values",
			inputRaw: map[string]interface{}{
				"set_attr": []interface{}{"valA", "valB"},
			},
			key:      "set_attr",
			expected: []string{"valA", "valB"},
		},
		{
			name:     "Non-existent key",
			inputRaw: map[string]interface{}{},
			key:      "missing",
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := schema.TestResourceDataRaw(t, resourceSchema, tt.inputRaw)
			actual := getStringList(d, tt.key)

			if len(actual) != len(tt.expected) {
				t.Fatalf("expected length %d, got %d", len(tt.expected), len(actual))
			}

			if tt.key == "list_attr" || tt.key == "missing" {
				if !reflect.DeepEqual(actual, tt.expected) {
					t.Errorf("expected %v, got %v", tt.expected, actual)
				}
			} else {
				actualMap := make(map[string]bool)
				for _, v := range actual {
					actualMap[v] = true
				}
				for _, v := range tt.expected {
					if !actualMap[v] {
						t.Errorf("expected value %s not found in actual list %v", v, actual)
					}
				}
			}
		})
	}
}

func TestParseZabbixDuration(t *testing.T) {
	cases := []struct {
		input     string
		expected  int
		expectErr bool
	}{
		{"30s", 30, false},
		{"5m", 300, false},
		{"2h", 7200, false},
		{"1d", 86400, false},
		{"1w", 604800, false},
		{"10w", 6048000, false},
		{"{$MACRO}", -1, false},
		{"abc", 0, true},
		{"-5", 0, true},
		{"11w", 0, true},
		{"71d", 0, true},
		{"99999999999w", 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			actual, err := parseZabbixDuration(tc.input)
			if tc.expectErr {
				if err == nil {
					t.Fatalf("expected error for %q, got none", tc.input)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error for %q: %s", tc.input, err)
				}
				if actual != tc.expected {
					t.Fatalf("expected duration %d, got %d", tc.expected, actual)
				}
			}
		})
	}
}
