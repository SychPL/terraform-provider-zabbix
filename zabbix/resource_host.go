package zabbix

import (
	"context"
	"errors"
	"fmt"

	"github.com/hashicorp/go-cty/cty"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
)

// discoveredError refuses objects created by low-level discovery (hosts and
// host groups both carry flags); they are owned by their discovery rule and
// the barrier must hold on EVERY mutating path (with -refresh=false Read
// never runs before apply or destroy). Read tolerates a missing flags field
// (objects from intermediaries), mutations use the fail-closed
// mutationFlagsError instead.
func discoveredError(kind, id, flags string) error {
	if flags == "" || flags == "0" {
		return nil
	}
	return fmt.Errorf("%s %s was created by low-level discovery (flags=%s) and cannot be managed by this provider; %s", kind, id, flags, unmanageableHint)
}

// mutationFlagsError is the fail-closed variant for Update and Delete: when
// the response carries no flags field at all, the LLD barrier cannot be
// verified and the mutation is refused rather than waved through.
func mutationFlagsError(kind, id, flags string) error {
	if flags == "0" {
		return nil
	}
	if flags == "" {
		return fmt.Errorf("%s %s: the API response carries no flags field, so the low-level-discovery barrier cannot be verified; refusing to mutate", kind, id)
	}
	return discoveredError(kind, id, flags)
}

// defaultAgentPort is the schema default for "port"; CustomizeDiff and Read
// reference it so the three places cannot drift apart.
const defaultAgentPort = "10050"

func resourceHost() *schema.Resource {
	return &schema.Resource{
		Description: "Manages a Zabbix host with a single main agent interface. " +
			"Other interfaces (SNMP, IPMI, JMX) that exist on the host are left untouched. " +
			"Hosts created by low-level discovery are refused - they are owned by their discovery rule. " +
			"Every attribute of this resource is managed authoritatively: after `terraform import`, reproduce the full configuration (including `enabled`, `name`, `description` and the interface address) and review the plan before the first apply - templates missing from the configuration are unlinked together with their inherited items and triggers.",
		CreateContext: resourceHostCreate,
		ReadContext:   resourceHostRead,
		UpdateContext: resourceHostUpdate,
		DeleteContext: resourceHostDelete,
		Importer:      passthroughImporter(),
		Timeouts:      defaultTimeouts(),
		CustomizeDiff: resourceHostCustomizeDiff,
		Schema: map[string]*schema.Schema{
			"host": {
				Type:         schema.TypeString,
				Required:     true,
				ValidateFunc: validation.StringIsNotWhiteSpace,
				Description:  "Technical name of the host. Must be unique in Zabbix.",
			},
			"name": {
				Type:        schema.TypeString,
				Optional:    true,
				Computed:    true,
				Description: "Visible name of the host. Defaults to `host`.",
			},
			"enabled": {
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     true,
				Description: "Whether the host is monitored.",
			},
			"description": {
				Type:        schema.TypeString,
				Optional:    true,
				Default:     "",
				Description: "Description of the host.",
			},
			"groups": {
				Type:        schema.TypeSet,
				Required:    true,
				MinItems:    1,
				Elem:        &schema.Schema{Type: schema.TypeString, ValidateFunc: validation.StringIsNotWhiteSpace},
				Description: "IDs of the host groups the host belongs to. At least one is required by Zabbix.",
			},
			"templates": {
				Type:        schema.TypeSet,
				Optional:    true,
				Elem:        &schema.Schema{Type: schema.TypeString, ValidateFunc: validation.StringIsNotWhiteSpace},
				Description: "IDs of templates linked to the host. Templates removed from this set are unlinked and their inherited entities cleared.",
			},
			"use_ip": {
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     true,
				Description: "Connect to the agent interface via `ip` (true) or `dns` (false).",
			},
			"ip": {
				Type:         schema.TypeString,
				Optional:     true,
				Default:      "",
				ValidateFunc: validateIP,
				Description:  "IP address of the main agent interface (or a user macro). Leave both `ip` and `dns` empty to create the host without any interface (e.g. for trapper or dependent items only).",
			},
			"dns": {
				Type:         schema.TypeString,
				Optional:     true,
				Default:      "",
				ValidateFunc: validateDNS,
				Description:  "DNS name of the main agent interface (or a user macro). Required when `use_ip` is false.",
			},
			"port": {
				Type:         schema.TypeString,
				Optional:     true,
				Default:      defaultAgentPort,
				ValidateFunc: validatePort,
				Description:  "Port of the main agent interface (number or user macro).",
			},
		},
	}
}

func resourceHostCustomizeDiff(_ context.Context, d *schema.ResourceDiff, _ interface{}) error {
	// The visible name follows the technical name unless configured explicitly;
	// normalising it here makes drift of a non-configured name visible in the plan.
	if planKnown(d, "host", "name") {
		raw := d.GetRawConfig()
		if d.Get("name").(string) == "" || (!raw.IsNull() && raw.Type().HasAttribute("name") && raw.GetAttr("name").IsNull()) {
			if err := d.SetNew("name", d.Get("host").(string)); err != nil {
				return err
			}
		}
	}
	if !planKnown(d, "use_ip", "ip", "dns", "port") {
		return nil // values with unknown references are re-validated in Create/Update
	}
	return validateHostAddress(d.GetRawConfig(), d.Get)
}

// validateHostAddress checks the interface address cross-rules. CustomizeDiff
// must skip unknown values (references to other resources), so Create and
// Update run the same checks again on the RESOLVED values; an address that
// was written in the configuration but resolved to an empty string is
// rejected instead of silently producing an agentless host.
func validateHostAddress(raw cty.Value, get func(string) interface{}) error {
	if !raw.IsNull() {
		for _, f := range []string{"ip", "dns"} {
			// Harness raw configs may be partial objects; a real Terraform core
			// always sends the full schema.
			if !raw.Type().HasAttribute(f) {
				continue
			}
			if v := raw.GetAttr(f); !v.IsNull() && v.IsKnown() {
				if s, _ := get(f).(string); s == "" {
					return fmt.Errorf("%s was configured but is empty; omit it to create the host without an agent interface", f)
				}
			}
		}
	}
	// The format validators are plan-time ValidateFuncs and are skipped for
	// unknown references; repeat them here on the RESOLVED values so a
	// malformed address fails before any mutation, not halfway through one.
	for _, f := range []struct {
		name string
		fn   func(interface{}, string) ([]string, []error)
	}{{"ip", validateIP}, {"dns", validateDNS}, {"port", validatePort}} {
		if _, errs := f.fn(get(f.name), f.name); len(errs) > 0 {
			return errs[0]
		}
	}
	ip, _ := get("ip").(string)
	dns, _ := get("dns").(string)
	useIP, _ := get("use_ip").(bool)
	port, _ := get("port").(string)
	if ip == "" && dns == "" {
		// No agent interface at all - valid for hosts monitored through
		// trapper/dependent items - but then DNS mode makes no sense and a
		// custom port would never be applied (perpetual diff).
		if !useIP {
			return fmt.Errorf("dns is required when use_ip is false")
		}
		if port != defaultAgentPort {
			return fmt.Errorf("port requires ip or dns (the host has no agent interface)")
		}
		return nil
	}
	if useIP && ip == "" {
		return fmt.Errorf("ip is required when use_ip is true (or set use_ip = false to connect via dns)")
	}
	if !useIP && dns == "" {
		return fmt.Errorf("dns is required when use_ip is false")
	}
	return nil
}

func expandHost(d *schema.ResourceData) *HostSpec {
	// The visible name follows the technical name unless configured. The
	// CustomizeDiff normalisation is skipped while `host` is unknown at plan
	// time, so it is re-derived here from the RESOLVED values: a stale name
	// from state must not survive a host rename.
	name := d.Get("name").(string)
	raw := d.GetRawConfig()
	nameConfigured := !raw.IsNull() && raw.Type().HasAttribute("name") && !raw.GetAttr("name").IsNull()
	if name == "" || !nameConfigured {
		name = d.Get("host").(string)
	}
	return &HostSpec{
		Host:        d.Get("host").(string),
		Name:        name,
		Status:      boolToStatus(d.Get("enabled").(bool)),
		Description: d.Get("description").(string),
		GroupIDs:    stringSet(d, "groups"),
		TemplateIDs: stringSet(d, "templates"),
		Interface: HostInterface{
			UseIP: boolToFlag(d.Get("use_ip").(bool)),
			IP:    d.Get("ip").(string),
			DNS:   d.Get("dns").(string),
			Port:  d.Get("port").(string),
		},
	}
}

func resourceHostCreate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	if err := validateHostAddress(d.GetRawConfig(), d.Get); err != nil {
		return diag.FromErr(err)
	}
	id, err := client.CreateHost(ctx, expandHost(d))
	if err != nil {
		return createError("host", d.Get("host").(string), err)
	}
	d.SetId(id)
	return readAfterCreate(ctx, d, m, resourceHostRead, "host")
}

// unmanageableHost refuses API shapes d.Set could not have accepted (an
// intermediary, a newer line): Read and the update preflight refuse exactly
// the same set instead of normalising unknown values to false/defaults.
func unmanageableHost(id string, host *Host) error {
	if host.Status != "0" && host.Status != "1" {
		return fmt.Errorf("host %s has status %q outside the supported 0/1 values; %s", id, host.Status, unmanageableHint)
	}
	iface := host.AgentInterface()
	if iface == nil {
		return nil
	}
	if iface.UseIP != "0" && iface.UseIP != "1" {
		return fmt.Errorf("host %s has interface useip %q outside the supported 0/1 values; %s", id, iface.UseIP, unmanageableHint)
	}
	for _, check := range []struct {
		validate     func(interface{}, string) ([]string, []error)
		value, field string
	}{
		{validateIP, iface.IP, "ip"},
		{validateDNS, iface.DNS, "dns"},
		{validatePort, iface.Port, "port"},
	} {
		if _, errs := check.validate(check.value, check.field); len(errs) > 0 {
			return fmt.Errorf("host %s interface: %s; %s", id, errs[0], unmanageableHint)
		}
	}
	return nil
}

func resourceHostRead(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	host, err := client.GetHost(ctx, d.Id())
	if err != nil {
		return readError(ctx, d, "host", err)
	}
	if err := discoveredError("host", d.Id(), host.Flags); err != nil {
		return diag.FromErr(err)
	}
	if err := unmanageableHost(d.Id(), host); err != nil {
		return diag.FromErr(err)
	}

	groups := make([]string, len(host.Groups))
	for i, g := range host.Groups {
		groups[i] = g.GroupID
	}
	templates := make([]string, len(host.ParentTemplates))
	for i, t := range host.ParentTemplates {
		templates[i] = t.TemplateID
	}

	values := map[string]interface{}{
		"host":        host.Host,
		"name":        host.Name,
		"enabled":     host.Status == "0",
		"description": host.Description,
		"groups":      groups,
		"templates":   templates,
	}
	if iface := host.AgentInterface(); iface != nil {
		values["use_ip"] = iface.UseIP == "1"
		values["ip"] = iface.IP
		values["dns"] = iface.DNS
		values["port"] = iface.Port
	} else {
		// No agent interface (imported SNMP-only host, or the interface was
		// removed outside Terraform): reflect that so the drift is visible; the
		// next apply recreates the interface from the configuration.
		tflog.Warn(ctx, fmt.Sprintf("host %s has no main agent interface", d.Id()))
		values["use_ip"] = true
		values["ip"] = ""
		values["dns"] = ""
		values["port"] = defaultAgentPort
	}
	if err := setFields(d, values); err != nil {
		return diag.FromErr(err)
	}
	return nil
}

func resourceHostUpdate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	// Partial mode from the very first statement: SDKv2 would otherwise write
	// the planned values into state even when validation or the preflight read
	// fails before any mutation (see ResourceData.Partial).
	d.Partial(true)
	if err := validateHostAddress(d.GetRawConfig(), d.Get); err != nil {
		return diag.FromErr(err)
	}
	host, err := client.GetHost(ctx, d.Id())
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return diag.Errorf("host %s vanished from Zabbix after the plan was created (deleted externally?); re-run terraform apply to refresh and recreate it", d.Id())
		}
		return diag.Errorf("reading host %s: %s", d.Id(), err)
	}
	if err := mutationFlagsError("host", d.Id(), host.Flags); err != nil {
		return diag.FromErr(err)
	}
	if err := unmanageableHost(d.Id(), host); err != nil {
		return diag.FromErr(err)
	}

	if d.HasChanges("host", "name", "enabled", "description", "groups", "templates") {
		// Templates to clear are computed from what Zabbix currently has linked
		// (not only from state) so that a template linked outside Terraform is
		// unlinked together with its inherited entities, as documented.
		spec := expandHost(d)
		current := make([]string, 0, len(host.ParentTemplates))
		for _, t := range host.ParentTemplates {
			current = append(current, t.TemplateID)
		}
		clear := stringsDiff(current, spec.TemplateIDs)
		if err := client.UpdateHost(ctx, d.Id(), spec, clear); err != nil {
			return diag.Errorf("updating host %s: %s", d.Id(), err)
		}
	}

	if d.HasChanges("use_ip", "ip", "dns", "port") {
		spec := expandHost(d).Interface
		iface := host.AgentInterface()
		switch {
		case !spec.HasAddress() && iface == nil:
			// nothing to do
		case !spec.HasAddress():
			// The interface was removed from the configuration. A parallel
			// automation may have deleted it between the preflight read and
			// this call: confirm the absence instead of failing the apply.
			err := deleteError(ctx, client.DeleteHostInterface(ctx, iface.InterfaceID), func(ctx context.Context) error {
				h, err := client.GetHost(ctx, d.Id())
				if err != nil {
					return err
				}
				if h.AgentInterface() == nil {
					return ErrNotFound
				}
				return nil
			})
			if err != nil {
				return diag.Errorf("removing agent interface of host %s: %s", d.Id(), err)
			}
		case iface != nil:
			spec.InterfaceID = iface.InterfaceID
			if err := client.UpdateHostInterface(ctx, spec); err != nil {
				if IsObjectMissing(err) {
					// A parallel automation deleted the interface between the
					// preflight read and this call - same friendly contract
					// as the other vanish paths.
					return diag.Errorf("agent interface of host %s vanished from Zabbix after the plan was created (deleted externally?); re-run terraform apply to recreate it", d.Id())
				}
				return diag.Errorf("updating host %s agent interface: %s", d.Id(), err)
			}
		default:
			// The agent interface is gone (removed outside Terraform or never
			// existed on an imported host): create it from the configuration.
			if err := client.CreateHostInterface(ctx, d.Id(), spec); err != nil {
				return diag.Errorf("creating agent interface for host %s: %s", d.Id(), err)
			}
		}
	}

	d.Partial(false)
	return resourceHostRead(ctx, d, m)
}

func resourceHostDelete(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	// The LLD barrier must hold here too: with -refresh=false Read never runs
	// before destroy, so the delete performs its own preflight.
	host, err := client.GetHost(ctx, d.Id())
	switch {
	case errors.Is(err, ErrNotFound):
		return nil // already gone - the delete is idempotent
	case err != nil:
		return diag.Errorf("reading host %s before delete: %s", d.Id(), err)
	}
	if err := mutationFlagsError("host", d.Id(), host.Flags); err != nil {
		return diag.FromErr(err)
	}

	err = deleteError(ctx, client.DeleteHost(ctx, d.Id()), func(ctx context.Context) error {
		_, err := client.GetHost(ctx, d.Id())
		return err
	})
	if err != nil {
		return diag.Errorf("deleting host %s: %s", d.Id(), err)
	}
	return nil
}
