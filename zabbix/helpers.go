package zabbix

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

// unknownMarker is the value the legacy SDK substitutes for unknown strings
// inside set elements (sets cannot carry per-element unknown-ness). The SDK
// defines it in an internal package (hcl2shim.UnknownVariableValue) that
// cannot be imported, hence this deliberate copy; the value is stable
// protocol surface in SDKv2.
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

// timePeriodRe matches the Zabbix time period format used by time-period
// conditions: "d-d,hh:mm-hh:mm" entries separated by semicolons, e.g.
// "1-7,00:00-24:00" or "1-5,09:00-18:00;6-7,10:00-16:00".
var timePeriodRe = regexp.MustCompile(`^[1-7](-[1-7])?,([01]?\d|2[0-3]):[0-5]\d-([01]?\d|2[0-4]):[0-5]\d(;[1-7](-[1-7])?,([01]?\d|2[0-3]):[0-5]\d-([01]?\d|2[0-4]):[0-5]\d)*;?$`)

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

// suppressEquivalentDuration treats equivalent spellings of the same duration
// ("3600", "3600s", "1h") as equal: the API may return a different spelling
// than the configuration, which would otherwise be a perpetual diff that
// never converges. Macros and unparsable values are never suppressed.
func suppressEquivalentDuration(_, old, new string, _ *schema.ResourceData) bool {
	o, err1 := parseZabbixDuration(old)
	n, err2 := parseZabbixDuration(new)
	return err1 == nil && err2 == nil && o == n && o >= 0
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
	if secs, err := parseZabbixDuration(v.(string)); err == nil && secs == 0 {
		return nil, nil // "0", "0s", "0m", ... - inherit from the action
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

// validateDNS accepts an empty value, a user macro or a DNS name (no
// whitespace, at most 255 characters); typos fail the plan, not the apply.
func validateDNS(v interface{}, k string) ([]string, []error) {
	s := v.(string)
	if s == "" || userMacroRe.MatchString(s) {
		return nil, nil
	}
	if len(s) > 255 || strings.ContainsAny(s, " \t") {
		return nil, []error{fmt.Errorf("%s: %q is not a valid DNS name or user macro", k, s)}
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

// createError classifies a failed create. A JSON-RPC error is definitive (the
// API rejected the request); anything else (transport error, timeout) leaves
// the outcome unknown - the request may still have executed, so the operator
// gets an import hint instead of an error that invites a duplicating retry.
func createError(kind, name string, err error) diag.Diagnostics {
	var rpcErr *JsonRpcError
	if errors.As(err, &rpcErr) {
		return diag.Errorf("creating %s: %s", kind, err)
	}
	return diag.Errorf("creating %s: %s; the outcome is unknown - if the request reached Zabbix, %q may exist without being tracked: check and import it before re-applying", kind, err, name)
}

// readAfterCreate runs the first Read after a successful create. An empty
// result there is a consistency error, not a deletion: forgetting the ID (the
// normal Read behaviour) would orphan the object that was just created, so
// the ID is kept and an error is returned instead.
func readAfterCreate(ctx context.Context, d *schema.ResourceData, m interface{}, read schema.ReadContextFunc, kind string) diag.Diagnostics {
	id := d.Id()
	diags := read(ctx, d, m)
	if d.Id() == "" && !diags.HasError() {
		d.SetId(id)
		return diag.Errorf("%s %s was created but the follow-up read returned no object; keeping it in state - check API consistency (stale replica/proxy) and re-run terraform", kind, id)
	}
	return diags
}
