package zabbix

import (
	"context"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

func resourceHostGroup() *schema.Resource {
	return &schema.Resource{
		CreateContext: resourceHostGroupCreate,
		ReadContext:   resourceHostGroupRead,
		UpdateContext: resourceHostGroupUpdate,
		DeleteContext: resourceHostGroupDelete,
		Schema: map[string]*schema.Schema{
			"name": {
				Type:        schema.TypeString,
				Required:    true,
				Description: "Name of the host group",
			},
		},
	}
}

func resourceHostGroupCreate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)
	name := d.Get("name").(string)

	groupID, err := client.CreateHostGroup(ctx, name)
	if err != nil {
		return diag.FromErr(err)
	}

	d.SetId(groupID)
	return resourceHostGroupRead(ctx, d, m)
}

func resourceHostGroupRead(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)
	id := d.Id()

	group, err := client.GetHostGroup(ctx, id)
	if err == ErrNotFound {
		d.SetId("")
		return nil
	}
	if err != nil {
		return diag.FromErr(err)
	}

	if err := d.Set("name", group.Name); err != nil {
		return diag.FromErr(err)
	}

	return nil
}

func resourceHostGroupUpdate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)
	id := d.Id()

	if d.HasChange("name") {
		name := d.Get("name").(string)
		if err := client.UpdateHostGroup(ctx, id, name); err != nil {
			return diag.FromErr(err)
		}
	}

	return resourceHostGroupRead(ctx, d, m)
}

func resourceHostGroupDelete(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)
	id := d.Id()

	if err := client.DeleteHostGroup(ctx, id); err != nil {
		return diag.FromErr(err)
	}

	d.SetId("")
	return nil
}
