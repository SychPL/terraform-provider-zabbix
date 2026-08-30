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
