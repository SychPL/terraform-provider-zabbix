package zabbix

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

// readError maps the common ErrNotFound error to a state-cleaning operation with a warning.
// Any other error is returned directly to prevent deleting resource state in case of connection errors.
func readError(ctx context.Context, d *schema.ResourceData, resourceName string, err error) diag.Diagnostics {
	if errors.Is(err, ErrNotFound) {
		tflog.Warn(ctx, fmt.Sprintf("%s %s not found in Zabbix, removing from state", resourceName, d.Id()))
		d.SetId("")
		return nil
	}
	return diag.FromErr(err)
}

// isDeleteSuccess checks if the Zabbix API returned a missing object error,
// confirming that the object is already gone, and treats it as a successful delete.
func isDeleteSuccess(err error) error {
	if err != nil && IsObjectMissing(err) {
		return nil
	}
	return err
}

// setStrings converts a TypeSet / TypeList parameter to a slice of strings.
func setStrings(v interface{}) []string {
	var items []interface{}
	switch val := v.(type) {
	case *schema.Set:
		items = val.List()
	case []interface{}:
		items = val
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

var userMacroRe = regexp.MustCompile(`^\{\$[A-Z0-9_.]+(:.*)?\}$`)

// parseZabbixDuration parses duration strings such as "30s", "1h", "90" (seconds).
// Returns -1 for Zabbix user macros.
func parseZabbixDuration(s string) (int, error) {
	if userMacroRe.MatchString(s) {
		return -1, nil
	}

	re := regexp.MustCompile(`^(\d+)([smhdw]?)$`)
	matches := re.FindStringSubmatch(s)
	if len(matches) != 3 {
		return 0, fmt.Errorf("invalid duration format: %q", s)
	}

	val, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0, fmt.Errorf("invalid duration value: %q", s)
	}

	switch matches[2] {
	case "s", "":
		return val, nil
	case "m":
		return val * 60, nil
	case "h":
		return val * 3600, nil
	case "d":
		return val * 86400, nil
	case "w":
		return val * 604800, nil
	default:
		return 0, fmt.Errorf("unsupported duration suffix: %q", matches[2])
	}
}

// validateEscPeriod checks that duration is between 60s and 1 week.
func validateEscPeriod(v interface{}, k string) ([]string, []error) {
	s := v.(string)
	secs, err := parseZabbixDuration(s)
	if err != nil {
		return nil, []error{fmt.Errorf("%s is invalid: %w", k, err)}
	}
	if secs != -1 && (secs < 60 || secs > 604800) {
		return nil, []error{fmt.Errorf("%s must be between 60s and 1w, got %q", k, s)}
	}
	return nil, nil
}

// validateOperationEscPeriod checks action operation steps, which can also be "0" (inheriting action period).
func validateOperationEscPeriod(v interface{}, k string) ([]string, []error) {
	s := v.(string)
	if s == "0" {
		return nil, nil
	}
	return validateEscPeriod(v, k)
}

// validatePort checks port is between 1 and 65535, or a user macro.
func validatePort(v interface{}, k string) ([]string, []error) {
	s := v.(string)
	if userMacroRe.MatchString(s) {
		return nil, nil
	}
	port, err := strconv.Atoi(s)
	if err != nil || port < 1 || port > 65535 {
		return nil, []error{fmt.Errorf("%s must be a port between 1 and 65535 or a user macro, got %q", k, s)}
	}
	return nil, nil
}
