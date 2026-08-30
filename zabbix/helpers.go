package zabbix

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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
		tflog.Warn(ctx, fmt.Sprintf("%s %s not found in Zabbix (or not visible to the current user); removing from state", kind, d.Id()))
		d.SetId("")
		return nil
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
	n, _ := strconv.Atoi(m[1])
	mult := map[string]int{"": 1, "s": 1, "m": 60, "h": 3600, "d": 86400, "w": 604800}
	return n * mult[m[2]], nil
}

// validateEscPeriod accepts 0 (use action default) or a duration of at least 60 seconds.
func validateEscPeriod(v interface{}, k string) ([]string, []error) {
	secs, err := parseZabbixDuration(v.(string))
	if err != nil {
		return nil, []error{fmt.Errorf("%s: %w", k, err)}
	}
	if secs != -1 && secs != 0 && secs < 60 {
		return nil, []error{fmt.Errorf("%s: must be 0 or at least 60 seconds, got %q", k, v)}
	}
	return nil, nil
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
