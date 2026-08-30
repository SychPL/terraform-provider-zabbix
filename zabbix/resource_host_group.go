package zabbix

import (
	"context"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
)

func resourceHostGroup() *schema.Resource {
	return &schema.Resource{
		Description: "Manages a Zabbix host group. The name is managed authoritatively: " +
			"after `terraform import`, set `name` to the current value and review the plan before the first apply - a different configured name renames the imported group.",
		CreateContext: resourceHostGroupCreate,
		ReadContext:   resourceHostGroupRead,
		UpdateContext: resourceHostGroupUpdate,
		DeleteContext: resourceHostGroupDelete,
		Importer:      passthroughImporter(),
		Timeouts:      defaultTimeouts(),
		Schema: map[string]*schema.Schema{
			"name": {
				Type:         schema.TypeString,
				Required:     true,
				ValidateFunc: validation.StringIsNotWhiteSpace,
				Description:  "Name of the host group. Must be unique in Zabbix.",
			},
		},
	}
}

func resourceHostGroupCreate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	id, err := client.CreateHostGroup(ctx, d.Get("name").(string))
	if err != nil {
		return diag.Errorf("creating host group: %s", err)
	}
	d.SetId(id)
	return readAfterCreate(ctx, d, m, resourceHostGroupRead, "host group")
}

func resourceHostGroupRead(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	group, err := client.GetHostGroup(ctx, d.Id())
	if err != nil {
		return readError(ctx, d, "host group", err)
	}
	if err := d.Set("name", group.Name); err != nil {
		return diag.FromErr(err)
	}
	return nil
}

func resourceHostGroupUpdate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	// SDKv2 writes the planned values into state even when Update fails (see
	// ResourceData.Partial); partial mode preserves the previous state until
	// the mutation is confirmed, then the final Read refreshes everything.
	d.Partial(true)
	if d.HasChange("name") {
		if err := client.UpdateHostGroup(ctx, d.Id(), d.Get("name").(string)); err != nil {
			return diag.Errorf("updating host group %s: %s", d.Id(), err)
		}
	}
	d.Partial(false)
	return resourceHostGroupRead(ctx, d, m)
}

func resourceHostGroupDelete(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	err := deleteError(ctx, client.DeleteHostGroup(ctx, d.Id()), func(ctx context.Context) error {
		_, err := client.GetHostGroup(ctx, d.Id())
		return err
	})
	if err != nil {
		return diag.Errorf("deleting host group %s: %s", d.Id(), err)
	}
	return nil
}
