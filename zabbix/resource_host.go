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
				Type:         schema.TypeString,
				Optional:     true,
				ValidateFunc: validateIP,
				Description:  "IP address of the host agent interface",
			},
			"dns": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "DNS name of the host agent interface",
			},
			"use_ip": {
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     true,
				Description: "Whether to connect to the host interface via IP (true) or DNS (false)",
			},
			"port": {
				Type:         schema.TypeString,
				Optional:     true,
				Default:      "10050",
				ValidateFunc: validatePort,
				Description:  "Main monitoring Port for the host agent interface",
			},
		},
	}
}

func resourceHostCreate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	host := d.Get("host").(string)
	groups := getStringList(d, "groups")
	templates := getStringList(d, "templates")

	useIPVal := d.Get("use_ip").(bool)
	useIP := "1"
	if !useIPVal {
		useIP = "0"
	}
	ip := d.Get("ip").(string)
	dns := d.Get("dns").(string)
	port := d.Get("port").(string)

	if ip == "" && dns == "" {
		// No interface requested, bypass validation
	} else {
		if useIPVal && ip == "" {
			return diag.Errorf("ip must be specified when use_ip is true")
		}
		if !useIPVal && dns == "" {
			return diag.Errorf("dns must be specified when use_ip is false")
		}
	}

	hostID, err := client.CreateHost(ctx, host, groups, templates, useIP, ip, dns, port)
	if err != nil {
		return diag.FromErr(err)
	}

	d.SetId(hostID)
	return resourceHostRead(ctx, d, m)
}

func resourceHostRead(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)
	id := d.Id()

	host, err := client.GetHost(ctx, id)
	if err != nil {
		return readError(ctx, d, "Host", err)
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

	// Find the main agent interface (type = 1 and main = 1)
	var mainInter *HostInterface
	for _, inter := range host.Interfaces {
		if inter.Type == "1" && inter.Main == "1" {
			mainInter = &inter
			break
		}
	}

	if mainInter != nil {
		d.Set("use_ip", mainInter.UseIP == "1")
		d.Set("ip", mainInter.IP)
		d.Set("dns", mainInter.DNS)
		d.Set("port", mainInter.Port)
	} else {
		d.Set("ip", "")
		d.Set("dns", "")
	}

	return nil
}

func resourceHostUpdate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)
	id := d.Id()

	if d.HasChanges("host", "groups", "templates") {
		host := d.Get("host").(string)
		groups := getStringList(d, "groups")

		oldRaw, newRaw := d.GetChange("templates")
		oldSet := oldRaw.(*schema.Set).List()
		newSet := newRaw.(*schema.Set).List()

		newMap := make(map[string]bool)
		var templates []string
		for _, v := range newSet {
			val := v.(string)
			templates = append(templates, val)
			newMap[val] = true
		}

		var templatesClear []string
		for _, v := range oldSet {
			val := v.(string)
			if !newMap[val] {
				templatesClear = append(templatesClear, val)
			}
		}

		if err := client.UpdateHost(ctx, id, host, groups, templates, templatesClear); err != nil {
			return diag.FromErr(err)
		}
	}

	if d.HasChanges("use_ip", "ip", "dns", "port") {
		useIPVal := d.Get("use_ip").(bool)
		useIP := "1"
		if !useIPVal {
			useIP = "0"
		}
		ip := d.Get("ip").(string)
		dns := d.Get("dns").(string)
		port := d.Get("port").(string)

		inter, err := client.GetHostInterface(ctx, id)
		if err != nil && err != ErrNotFound {
			return diag.Errorf("failed to retrieve host interface: %s", err)
		}

		if ip == "" && dns == "" {
			// User wants no interface
			if err == nil {
				// Interface exists, delete it
				if err := client.DeleteHostInterface(ctx, inter.InterfaceID); err != nil {
					return diag.Errorf("failed to delete host interface: %s", err)
				}
			}
		} else {
			// User wants an interface
			if useIPVal && ip == "" {
				return diag.Errorf("ip must be specified when use_ip is true")
			}
			if !useIPVal && dns == "" {
				return diag.Errorf("dns must be specified when use_ip is false")
			}

			if err == ErrNotFound {
				// Interface does not exist, create it
				if err := client.CreateHostInterface(ctx, id, useIP, ip, dns, port); err != nil {
					return diag.Errorf("failed to create host interface: %s", err)
				}
			} else {
				// Interface exists, update it
				inter.UseIP = useIP
				inter.IP = ip
				inter.DNS = dns
				inter.Port = port

				if err := client.UpdateHostInterface(ctx, inter); err != nil {
					return diag.Errorf("failed to update host interface: %s", err)
				}
			}
		}
	}

	return resourceHostRead(ctx, d, m)
}

func resourceHostDelete(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)
	id := d.Id()

	err := client.DeleteHost(ctx, id)
	if isDeleteSuccess(err) != nil {
		return diag.FromErr(err)
	}

	d.SetId("")
	return nil
}

// Helper to convert set/list interfaces to string slices
func getStringList(d *schema.ResourceData, key string) []string {
	var list []string
	v, ok := d.GetOk(key)
	if !ok {
		return list
	}
	switch val := v.(type) {
	case []interface{}:
		return appendItems(list, val)
	case *schema.Set:
		return appendItems(list, val.List())
	}
	return list
}

func appendItems(list []string, items []interface{}) []string {
	for _, item := range items {
		if str, ok := item.(string); ok {
			list = append(list, str)
		}
	}
	return list
}
