package zabbix

import (
	"context"
	"strconv"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

func resourceMediaType() *schema.Resource {
	return &schema.Resource{
		CreateContext: resourceMediaTypeCreate,
		ReadContext:   resourceMediaTypeRead,
		UpdateContext: resourceMediaTypeUpdate,
		DeleteContext: resourceMediaTypeDelete,
		Schema: map[string]*schema.Schema{
			"name": {
				Type:        schema.TypeString,
				Required:    true,
				Description: "The name of the media type",
			},
			"type": {
				Type:        schema.TypeInt,
				Required:    true,
				Description: "Media type: 0 - Email, 1 - Script, 2 - SMS, 4 - Webhook",
			},
			"enabled": {
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     true,
				Description: "Whether the media type is enabled",
			},
			"smtp_server": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "SMTP server address (required for Email type)",
			},
			"smtp_helo": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "SMTP HELO (required for Email type)",
			},
			"smtp_email": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "SMTP email (required for Email type)",
			},
			"exec_path": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "Script name (required for Script type)",
			},
			"gsm_modem": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "GSM modem serial port (required for SMS type)",
			},
			"script": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "JavaScript code (required for Webhook type)",
			},
			"timeout": {
				Type:        schema.TypeString,
				Optional:    true,
				Default:     "30s",
				Description: "Webhook execution timeout",
			},
			"parameter": {
				Type:        schema.TypeList,
				Optional:    true,
				Description: "List of parameters (for Webhooks)",
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"name": {
							Type:     schema.TypeString,
							Required: true,
						},
						"value": {
							Type:     schema.TypeString,
							Required: true,
						},
					},
				},
			},
		},
	}
}

func resourceMediaTypeCreate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	mt := expandMediaType(d)
	mediaTypeID, err := client.CreateMediaType(mt)
	if err != nil {
		return diag.FromErr(err)
	}

	d.SetId(mediaTypeID)
	return resourceMediaTypeRead(ctx, d, m)
}

func resourceMediaTypeRead(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)
	id := d.Id()

	mt, err := client.GetMediaType(id)
	if err != nil {
		d.SetId("")
		return nil
	}

	d.Set("name", mt.Name)
	t, _ := strconv.Atoi(mt.Type)
	d.Set("type", t)
	d.Set("enabled", mt.Status == "0")
	d.Set("smtp_server", mt.SMTPServer)
	d.Set("smtp_helo", mt.SMTPHelo)
	d.Set("smtp_email", mt.SMTPEmail)
	d.Set("exec_path", mt.ExecPath)
	d.Set("gsm_modem", mt.GSMModem)
	d.Set("script", mt.Script)
	d.Set("timeout", mt.Timeout)

	// Read parameters
	if len(mt.Parameters) > 0 {
		params := make([]map[string]interface{}, len(mt.Parameters))
		for i, p := range mt.Parameters {
			params[i] = map[string]interface{}{
				"name":  p.Name,
				"value": p.Value,
			}
		}
		d.Set("parameter", params)
	}

	return nil
}

func resourceMediaTypeUpdate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	mt := expandMediaType(d)
	mt.MediaTypeID = d.Id()

	if err := client.UpdateMediaType(mt); err != nil {
		return diag.FromErr(err)
	}

	return resourceMediaTypeRead(ctx, d, m)
}

func resourceMediaTypeDelete(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)
	id := d.Id()

	if err := client.DeleteMediaType(id); err != nil {
		return diag.FromErr(err)
	}

	d.SetId("")
	return nil
}

func expandMediaType(d *schema.ResourceData) *MediaType {
	status := "0" // Enabled
	if !d.Get("enabled").(bool) {
		status = "1" // Disabled
	}

	mt := &MediaType{
		Name:        d.Get("name").(string),
		Type:        strconv.Itoa(d.Get("type").(int)),
		Status:      status,
		SMTPServer:  d.Get("smtp_server").(string),
		SMTPHelo:    d.Get("smtp_helo").(string),
		SMTPEmail:   d.Get("smtp_email").(string),
		ExecPath:    d.Get("exec_path").(string),
		GSMModem:    d.Get("gsm_modem").(string),
		Script:      d.Get("script").(string),
		Timeout:     d.Get("timeout").(string),
	}

	if rawParams, ok := d.GetOk("parameter"); ok {
		rawList := rawParams.([]interface{})
		params := make([]MediaTypeParam, len(rawList))
		for i, item := range rawList {
			m := item.(map[string]interface{})
			params[i] = MediaTypeParam{
				Name:  m["name"].(string),
				Value: m["value"].(string),
			}
		}
		mt.Parameters = params
	}

	return mt
}
