package zabbix

import (
	"context"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

func resourceHost() *schema.Resource {
	return &schema.Resource{
		CreateContext: resourceHostCreate,
		ReadContext:   resourceHostRead,
		UpdateContext: resourceHostUpdate,
		DeleteContext: resourceHostDelete,
		Schema: map[string]*schema.Schema{
			"host": {
				Type:        schema.TypeString,
				Required:    true,
				Description: "Technical name of the host",
			},
			"groups": {
				Type:        schema.TypeSet,
				Required:    true,
				Elem:        &schema.Schema{Type: schema.TypeString},
				Description: "List of Host Group IDs to assign to this host",
			},
			"templates": {
				Type:        schema.TypeSet,
				Optional:    true,
				Elem:        &schema.Schema{Type: schema.TypeString},
				Description: "List of Zabbix Template IDs to link to this host",
			},
			"ip": {
				Type:        schema.TypeString,
				Required:    true,
				Description: "Main monitoring IP address for the host agent interface",
			},
			"port": {
				Type:        schema.TypeString,
				Optional:    true,
				Default:     "10050",
				Description: "Main monitoring Port for the host agent interface",
			},
		},
	}
}

func resourceHostCreate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	host := d.Get("host").(string)
	groups := getStringList(d, "groups")
	templates := getStringList(d, "templates")
	ip := d.Get("ip").(string)
	port := d.Get("port").(string)

	hostID, err := client.CreateHost(host, groups, templates, ip, port)
	if err != nil {
		return diag.FromErr(err)
	}

	d.SetId(hostID)
	return resourceHostRead(ctx, d, m)
}

func resourceHostRead(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)
	id := d.Id()

	host, err := client.GetHost(id)
	if err != nil {
		d.SetId("")
		return nil
	}

	if err := d.Set("host", host.Host); err != nil {
		return diag.FromErr(err)
	}

	// Read group IDs
	groupIds := make([]string, len(host.Groups))
	for i, g := range host.Groups {
		groupIds[i] = g.GroupID
	}
	if err := d.Set("groups", groupIds); err != nil {
		return diag.FromErr(err)
	}

	// Read template IDs
	templateIds := make([]string, len(host.ParentTemplates))
	for i, t := range host.ParentTemplates {
		templateIds[i] = t.TemplateID
	}
	if err := d.Set("templates", templateIds); err != nil {
		return diag.FromErr(err)
	}

	// Read interface IP and Port (from the first interface)
	if len(host.Interfaces) > 0 {
		if err := d.Set("ip", host.Interfaces[0].IP); err != nil {
			return diag.FromErr(err)
		}
		if err := d.Set("port", host.Interfaces[0].Port); err != nil {
			return diag.FromErr(err)
		}
	}

	return nil
}

func resourceHostUpdate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)
	id := d.Id()

	if d.HasChanges("host", "groups", "templates", "ip", "port") {
		host := d.Get("host").(string)
		groups := getStringList(d, "groups")
		templates := getStringList(d, "templates")
		ip := d.Get("ip").(string)
		port := d.Get("port").(string)

		if err := client.UpdateHost(id, host, groups, templates, ip, port); err != nil {
			return diag.FromErr(err)
		}
	}

	return resourceHostRead(ctx, d, m)
}

func resourceHostDelete(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)
	id := d.Id()

	if err := client.DeleteHost(id); err != nil {
		return diag.FromErr(err)
	}

	d.SetId("")
	return nil
}

// Helper to convert set/list interfaces to string slices
func getStringList(d *schema.ResourceData, key string) []string {
	var list []string
	if v, ok := d.GetOk(key); ok {
		switch val := v.(type) {
		case []interface{}:
			for _, item := range val {
				if str, ok := item.(string); ok {
					list = append(list, str)
				}
			}
		case *schema.Set:
			for _, item := range val.List() {
				if str, ok := item.(string); ok {
					list = append(list, str)
				}
			}
		}
	}
	return list
}
