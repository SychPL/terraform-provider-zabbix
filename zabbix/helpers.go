package zabbix

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

// unknownMarker is the value the legacy SDK substitutes for unknown strings
// inside set elements (sets cannot carry per-element unknown-ness).
const unknownMarker = "74D93920-ED26-11E3-AC10-0800200C9A66"

func isUnknownMarker(s string) bool { return s == unknownMarker }

// planKnown reports whether every listed attribute is known in the plan.
// Cross-attribute validation only runs on known values; values referencing
// other resources are validated by Zabbix at apply time.
func planKnown(d *schema.ResourceDiff, keys ...string) bool {
	for _, k := range keys {
		// For sets/lists the SDK reports the unknown-ness on the count key only.
		if !d.NewValueKnown(k) || !d.NewValueKnown(k+".#") {
			return false
		}
	}
	return true
}

func defaultTimeouts() *schema.ResourceTimeout {
	t := schema.DefaultTimeout(2 * time.Minute)
	return &schema.ResourceTimeout{Create: t, Read: t, Update: t, Delete: t}
}

func passthroughImporter() *schema.ResourceImporter {
	return &schema.ResourceImporter{StateContext: schema.ImportStatePassthroughContext}
}

// setStrings converts a TypeSet / TypeList of strings to a slice.
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

func stringSet(d *schema.ResourceData, key string) []string {
	return setStrings(d.Get(key))
}

// stringsDiff returns the elements of a that are not in b.
func stringsDiff(a, b []string) []string {
	present := make(map[string]struct{}, len(b))
	for _, s := range b {
		present[s] = struct{}{}
	}
	var out []string
	for _, s := range a {
		if _, ok := present[s]; !ok {
			out = append(out, s)
		}
	}
	return out
}

func boolToStatus(enabled bool) string {
	if enabled {
		return "0"
	}
	return "1"
}

func boolToFlag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func atoi(field, s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("unexpected non-numeric value %q for %s from Zabbix API", s, field)
	}
	return n, nil
}

// setFields applies every value and stops at the first error.
func setFields(d *schema.ResourceData, values map[string]interface{}) error {
	for k, v := range values {
		if err := d.Set(k, v); err != nil {
			return fmt.Errorf("setting %s: %w", k, err)
		}
	}
	return nil
}

// readError handles the error of a Get* call inside a Read function. A
// confirmed "not found" removes the resource from state with a warning; any
// other error is surfaced and the state is left untouched.
func readError(ctx context.Context, d *schema.ResourceData, kind string, err error) diag.Diagnostics {
	if errors.Is(err, ErrNotFound) {
		msg := fmt.Sprintf("%s %s not found in Zabbix (or not visible to the current user); removing it from state, it will be recreated on the next apply", kind, d.Id())
		tflog.Warn(ctx, msg)
		d.SetId("")
		return diag.Diagnostics{{Severity: diag.Warning, Summary: "Resource removed from state", Detail: msg}}
	}
	return diag.Errorf("reading %s %s: %s", kind, d.Id(), err)
}

// deleteError makes Delete idempotent: when Zabbix reports that the referred
// object does not exist, the absence is confirmed with a Get before the
// deletion is treated as successful.
func deleteError(ctx context.Context, err error, confirm func(context.Context) error) error {
	if err == nil || !IsObjectMissing(err) {
		return err
	}
	if getErr := confirm(ctx); errors.Is(getErr, ErrNotFound) {
		return nil
	}
	return err
}

var userMacroRe = regexp.MustCompile(`^\{\$[A-Z0-9_.]+(:.*)?\}$`)
var durationRe = regexp.MustCompile(`^(\d+)([smhdw]?)$`)

// parseZabbixDuration parses a Zabbix time suffix value ("30s", "1h", "90")
// into seconds. Returns -1 for user macros (which cannot be validated).
func parseZabbixDuration(s string) (int, error) {
	if userMacroRe.MatchString(s) {
		return -1, nil
	}
	m := durationRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%q is not a valid Zabbix duration (e.g. 90, 30s, 5m, 1h, 1d, 1w) or user macro", s)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("%q is out of range", s)
	}
	mult := map[string]int{"": 1, "s": 1, "m": 60, "h": 3600, "d": 86400, "w": 604800}
	const maxSeconds = 10 * 604800 // far above any value Zabbix accepts
	if n > maxSeconds/mult[m[2]] {
		return 0, fmt.Errorf("%q is out of range", s)
	}
	return n * mult[m[2]], nil
}

// validateEscPeriod accepts a duration between 60 seconds and 1 week (or a
// user macro); used for the action's default step duration.
func validateEscPeriod(v interface{}, k string) ([]string, []error) {
	secs, err := parseZabbixDuration(v.(string))
	if err != nil {
		return nil, []error{fmt.Errorf("%s: %w", k, err)}
	}
	if secs != -1 && (secs < 60 || secs > 604800) {
		return nil, []error{fmt.Errorf("%s: must be between 60 seconds and 1 week, got %q", k, v)}
	}
	return nil, nil
}

// validateOperationEscPeriod additionally accepts 0 (inherit the action's period).
func validateOperationEscPeriod(v interface{}, k string) ([]string, []error) {
	if v.(string) == "0" {
		return nil, nil
	}
	return validateEscPeriod(v, k)
}

// validateIP accepts an empty value, an IP address or a user macro.
func validateIP(v interface{}, k string) ([]string, []error) {
	s := v.(string)
	if s == "" || userMacroRe.MatchString(s) || net.ParseIP(s) != nil {
		return nil, nil
	}
	return nil, []error{fmt.Errorf("%s: %q is not a valid IP address or user macro", k, s)}
}

// validatePort accepts a port number 1-65535 or a user macro.
func validatePort(v interface{}, k string) ([]string, []error) {
	s := v.(string)
	if userMacroRe.MatchString(s) {
		return nil, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return nil, []error{fmt.Errorf("%s: must be a port number 1-65535 or a user macro, got %q", k, s)}
	}
	return nil, nil
}
