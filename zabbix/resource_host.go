package zabbix

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
)

func resourceHost() *schema.Resource {
	return &schema.Resource{
		Description: "Manages a Zabbix host with a single main agent interface. " +
			"Other interfaces (SNMP, IPMI, JMX) that exist on the host are left untouched.",
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
				Elem:        &schema.Schema{Type: schema.TypeString},
				Description: "IDs of the host groups the host belongs to. At least one is required by Zabbix.",
			},
			"templates": {
				Type:        schema.TypeSet,
				Optional:    true,
				Elem:        &schema.Schema{Type: schema.TypeString},
				Description: "IDs of templates linked to the host. Templates removed from this set are unlinked and their inherited entities cleared.",
			},
			"use_ip": {
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     true,
				Description: "Connect to the agent interface via `ip` (true) or `dns` (false).",
			},
			"ip": {
				Type:        schema.TypeString,
				Optional:    true,
				Default:     "",
				Description: "IP address of the main agent interface. Required when `use_ip` is true.",
			},
			"dns": {
				Type:        schema.TypeString,
				Optional:    true,
				Default:     "",
				Description: "DNS name of the main agent interface. Required when `use_ip` is false.",
			},
			"port": {
				Type:         schema.TypeString,
				Optional:     true,
				Default:      "10050",
				ValidateFunc: validatePort,
				Description:  "Port of the main agent interface (number or user macro).",
			},
		},
	}
}

func resourceHostCustomizeDiff(_ context.Context, d *schema.ResourceDiff, _ interface{}) error {
	if d.Get("use_ip").(bool) {
		if d.Get("ip").(string) == "" {
			return fmt.Errorf("ip is required when use_ip is true")
		}
	} else if d.Get("dns").(string) == "" {
		return fmt.Errorf("dns is required when use_ip is false")
	}
	return nil
}

func expandHost(d *schema.ResourceData) *HostSpec {
	name := d.Get("name").(string)
	if name == "" {
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

	id, err := client.CreateHost(ctx, expandHost(d))
	if err != nil {
		return diag.Errorf("creating host: %s", err)
	}
	d.SetId(id)
	return resourceHostRead(ctx, d, m)
}

func resourceHostRead(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	host, err := client.GetHost(ctx, d.Id())
	if err != nil {
		return readError(ctx, d, "host", err)
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
		tflog.Warn(ctx, fmt.Sprintf("host %s has no main agent interface; interface attributes are not refreshed", d.Id()))
	}
	if err := setFields(d, values); err != nil {
		return diag.FromErr(err)
	}
	return nil
}

func resourceHostUpdate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	if d.HasChanges("host", "name", "enabled", "description", "groups", "templates") {
		oldT, newT := d.GetChange("templates")
		clear := stringsDiff(setStrings(oldT), setStrings(newT))
		if err := client.UpdateHost(ctx, d.Id(), expandHost(d), clear); err != nil {
			return diag.Errorf("updating host %s: %s", d.Id(), err)
		}
	}

	if d.HasChanges("use_ip", "ip", "dns", "port") {
		host, err := client.GetHost(ctx, d.Id())
		if err != nil {
			return diag.Errorf("reading host %s interfaces: %s", d.Id(), err)
		}
		iface := host.AgentInterface()
		if iface == nil {
			return diag.Errorf("host %s has no main agent interface to update; add one in Zabbix or remove the interface attributes from the configuration", d.Id())
		}
		spec := expandHost(d).Interface
		spec.InterfaceID = iface.InterfaceID
		if err := client.UpdateHostInterface(ctx, spec); err != nil {
			return diag.Errorf("updating host %s agent interface: %s", d.Id(), err)
		}
	}

	return resourceHostRead(ctx, d, m)
}

func resourceHostDelete(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	err := deleteError(ctx, client.DeleteHost(ctx, d.Id()), func(ctx context.Context) error {
		_, err := client.GetHost(ctx, d.Id())
		return err
	})
	if err != nil {
		return diag.Errorf("deleting host %s: %s", d.Id(), err)
	}
	return nil
}
