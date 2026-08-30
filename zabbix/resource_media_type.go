package zabbix

import (
	"context"
	"fmt"
	"reflect"
	"strconv"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
)

const (
	mediaTypeEmail   = 0
	mediaTypeScript  = 1
	mediaTypeSMS     = 2
	mediaTypeWebhook = 4
)

// mediaTypeSchema is shared with CustomizeDiff, which compares planned values
// against the schema defaults.
var mediaTypeSchema = resourceMediaTypeSchema()

func resourceMediaType() *schema.Resource {
	return &schema.Resource{
		Description: "Manages a Zabbix media type (email, script, SMS or webhook). " +
			"Only the attributes relevant for the selected `type` are sent to Zabbix.",
		CreateContext: resourceMediaTypeCreate,
		ReadContext:   resourceMediaTypeRead,
		UpdateContext: resourceMediaTypeUpdate,
		DeleteContext: resourceMediaTypeDelete,
		Importer:      passthroughImporter(),
		Timeouts:      defaultTimeouts(),
		CustomizeDiff: resourceMediaTypeCustomizeDiff,
		Schema:        mediaTypeSchema,
	}
}

func resourceMediaTypeSchema() map[string]*schema.Schema {
	return map[string]*schema.Schema{
		"name": {
			Type:         schema.TypeString,
			Required:     true,
			ValidateFunc: validation.StringIsNotWhiteSpace,
			Description:  "Name of the media type. Must be unique in Zabbix.",
		},
		"type": {
			Type:         schema.TypeInt,
			Required:     true,
			ValidateFunc: validation.IntInSlice([]int{mediaTypeEmail, mediaTypeScript, mediaTypeSMS, mediaTypeWebhook}),
			Description:  "Transport: 0 - Email, 1 - Script, 2 - SMS, 4 - Webhook.",
		},
		"enabled": {
			Type:        schema.TypeBool,
			Optional:    true,
			Default:     true,
			Description: "Whether the media type is enabled.",
		},
		// Email
		"smtp_server": {
			Type:        schema.TypeString,
			Optional:    true,
			Default:     "",
			Description: "SMTP server address. Required for type 0 (Email).",
		},
		"smtp_port": {
			Type:         schema.TypeInt,
			Optional:     true,
			Default:      25,
			ValidateFunc: validation.IsPortNumber,
			Description:  "SMTP server port (Email).",
		},
		"smtp_helo": {
			Type:        schema.TypeString,
			Optional:    true,
			Default:     "",
			Description: "SMTP HELO. Required for type 0 (Email).",
		},
		"smtp_email": {
			Type:        schema.TypeString,
			Optional:    true,
			Default:     "",
			Description: "Sender email address. Required for type 0 (Email).",
		},
		"smtp_security": {
			Type:         schema.TypeInt,
			Optional:     true,
			Default:      0,
			ValidateFunc: validation.IntInSlice([]int{0, 1, 2}),
			Description:  "SMTP connection security: 0 - none, 1 - STARTTLS, 2 - SSL/TLS (Email).",
		},
		"smtp_verify_peer": {
			Type:        schema.TypeBool,
			Optional:    true,
			Default:     false,
			Description: "Verify the SMTP server certificate (Email).",
		},
		"smtp_verify_host": {
			Type:        schema.TypeBool,
			Optional:    true,
			Default:     false,
			Description: "Verify the SMTP server host name in the certificate (Email).",
		},
		"smtp_authentication": {
			Type:         schema.TypeInt,
			Optional:     true,
			Default:      0,
			ValidateFunc: validation.IntInSlice([]int{0, 1}),
			Description:  "SMTP authentication: 0 - none, 1 - normal password (Email).",
		},
		"username": {
			Type:        schema.TypeString,
			Optional:    true,
			Default:     "",
			Description: "SMTP user name. Only sent when `smtp_authentication` is 1 (Email).",
		},
		"password": {
			Type:        schema.TypeString,
			Optional:    true,
			Sensitive:   true,
			Default:     "",
			Description: "SMTP password. Only sent when `smtp_authentication` is 1 (Email). Stored in the Terraform state; protect the state accordingly.",
		},
		// Script
		"exec_path": {
			Type:        schema.TypeString,
			Optional:    true,
			Default:     "",
			Description: "Script name. Required for type 1 (Script).",
		},
		// SMS
		"gsm_modem": {
			Type:        schema.TypeString,
			Optional:    true,
			Default:     "",
			Description: "GSM modem serial device. Required for type 2 (SMS).",
		},
		// Webhook
		"script": {
			Type:        schema.TypeString,
			Optional:    true,
			Default:     "",
			Description: "JavaScript webhook body. Required for type 4 (Webhook). Keep secrets in `parameter` values, not in the script.",
		},
		"timeout": {
			Type:        schema.TypeString,
			Optional:    true,
			Default:     "30s",
			Description: "Webhook execution timeout, 1-60s (Webhook).",
		},
		"parameter": {
			Type:        schema.TypeList,
			Optional:    true,
			Description: "Webhook input parameters (type 4 only). Values are marked sensitive.",
			Elem: &schema.Resource{
				Schema: map[string]*schema.Schema{
					"name": {
						Type:        schema.TypeString,
						Required:    true,
						Description: "Parameter name.",
					},
					"value": {
						Type:        schema.TypeString,
						Required:    true,
						Sensitive:   true,
						Description: "Parameter value (may contain macros).",
					},
				},
			},
		},
	}
}

// mediaTypeFields lists the attributes that belong to each media type type.
var mediaTypeFields = map[int][]string{
	mediaTypeEmail:   {"smtp_server", "smtp_port", "smtp_helo", "smtp_email", "smtp_security", "smtp_verify_peer", "smtp_verify_host", "smtp_authentication", "username", "password"},
	mediaTypeScript:  {"exec_path"},
	mediaTypeSMS:     {"gsm_modem"},
	mediaTypeWebhook: {"script", "timeout", "parameter"},
}

func resourceMediaTypeCustomizeDiff(_ context.Context, d *schema.ResourceDiff, _ interface{}) error {
	if !planKnown(d, "type") {
		return nil
	}
	t := d.Get("type").(int)
	known := func(f string) bool { return planKnown(d, f) }

	// Attributes of other types set to a non-default value would be silently
	// dropped (they are cleared in Zabbix and reset by Read); reject them.
	// Unknown values are always "set" - a reference to another resource is an
	// explicit configuration.
	allowed := map[string]bool{}
	for _, f := range mediaTypeFields[t] {
		allowed[f] = true
	}
	for _, fields := range mediaTypeFields {
		for _, f := range fields {
			if allowed[f] {
				continue
			}
			set := !known(f)
			if known(f) {
				if f == "parameter" {
					set = len(d.Get(f).([]interface{})) > 0
				} else {
					set = !reflect.DeepEqual(d.Get(f), mediaTypeSchema[f].Default)
				}
			}
			if set {
				return fmt.Errorf("%s is not supported for media type %d and would be ignored", f, t)
			}
		}
	}

	require := func(field string) error {
		if known(field) && d.Get(field).(string) == "" {
			return fmt.Errorf("%s is required for media type %d", field, t)
		}
		return nil
	}
	switch t {
	case mediaTypeEmail:
		for _, f := range []string{"smtp_server", "smtp_helo", "smtp_email"} {
			if err := require(f); err != nil {
				return err
			}
		}
		if known("smtp_authentication") && d.Get("smtp_authentication").(int) == 0 {
			for _, f := range []string{"username", "password"} {
				if known(f) && d.Get(f).(string) != "" {
					return fmt.Errorf("username/password require smtp_authentication = 1")
				}
			}
		}
	case mediaTypeScript:
		return require("exec_path")
	case mediaTypeSMS:
		return require("gsm_modem")
	case mediaTypeWebhook:
		if err := require("script"); err != nil {
			return err
		}
		if known("timeout") {
			secs, err := parseZabbixDuration(d.Get("timeout").(string))
			if err != nil {
				return fmt.Errorf("timeout: %w", err)
			}
			if secs < 1 || secs > 60 { // the API accepts 1-60s only, no macros
				return fmt.Errorf("timeout must be between 1s and 60s")
			}
		}
	}
	return nil
}

func expandMediaType(d *schema.ResourceData) *MediaType {
	mt := &MediaType{
		Name:               d.Get("name").(string),
		Type:               strconv.Itoa(d.Get("type").(int)),
		Status:             boolToStatus(d.Get("enabled").(bool)),
		SMTPServer:         d.Get("smtp_server").(string),
		SMTPPort:           strconv.Itoa(d.Get("smtp_port").(int)),
		SMTPHelo:           d.Get("smtp_helo").(string),
		SMTPEmail:          d.Get("smtp_email").(string),
		SMTPSecurity:       strconv.Itoa(d.Get("smtp_security").(int)),
		SMTPVerifyPeer:     boolToFlag(d.Get("smtp_verify_peer").(bool)),
		SMTPVerifyHost:     boolToFlag(d.Get("smtp_verify_host").(bool)),
		SMTPAuthentication: strconv.Itoa(d.Get("smtp_authentication").(int)),
		Username:           d.Get("username").(string),
		Passwd:             d.Get("password").(string),
		ExecPath:           d.Get("exec_path").(string),
		GSMModem:           d.Get("gsm_modem").(string),
		Script:             d.Get("script").(string),
		Timeout:            d.Get("timeout").(string),
		Parameters:         []MediaTypeParam{},
	}
	for _, item := range d.Get("parameter").([]interface{}) {
		p := item.(map[string]interface{})
		mt.Parameters = append(mt.Parameters, MediaTypeParam{Name: p["name"].(string), Value: p["value"].(string)})
	}
	return mt
}

func resourceMediaTypeCreate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	id, err := client.CreateMediaType(ctx, expandMediaType(d))
	if err != nil {
		return diag.Errorf("creating media type: %s", err)
	}
	d.SetId(id)
	return resourceMediaTypeRead(ctx, d, m)
}

func resourceMediaTypeRead(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	mt, err := client.GetMediaType(ctx, d.Id())
	if err != nil {
		return readError(ctx, d, "media type", err)
	}

	ints := map[string]int{}
	for field, raw := range map[string]string{
		"type": mt.Type, "smtp_port": mt.SMTPPort, "smtp_security": mt.SMTPSecurity, "smtp_authentication": mt.SMTPAuthentication,
	} {
		n, err := atoi(field, raw)
		if err != nil {
			return diag.FromErr(err)
		}
		ints[field] = n
	}

	if ints["type"] == mediaTypeScript && len(mt.Parameters) > 0 {
		return diag.Errorf("media type %s is a script media type with parameters, which this provider does not support; %s", d.Id(), unmanageableHint)
	}
	params := make([]map[string]interface{}, len(mt.Parameters))
	for i, p := range mt.Parameters {
		params[i] = map[string]interface{}{"name": p.Name, "value": p.Value}
	}

	// Only attributes relevant for the type are taken from the API; the rest are
	// reset to their schema defaults so that stale values left in Zabbix after a
	// type change do not produce a permanent diff.
	values := map[string]interface{}{
		"name":                mt.Name,
		"type":                ints["type"],
		"enabled":             mt.Status == "0",
		"smtp_server":         "",
		"smtp_port":           25,
		"smtp_helo":           "",
		"smtp_email":          "",
		"smtp_security":       0,
		"smtp_verify_peer":    false,
		"smtp_verify_host":    false,
		"smtp_authentication": 0,
		"username":            "",
		"password":            "",
		"exec_path":           "",
		"gsm_modem":           "",
		"script":              "",
		"timeout":             "30s",
		"parameter":           []map[string]interface{}{},
	}
	switch ints["type"] {
	case mediaTypeEmail:
		values["smtp_server"] = mt.SMTPServer
		values["smtp_port"] = ints["smtp_port"]
		values["smtp_helo"] = mt.SMTPHelo
		values["smtp_email"] = mt.SMTPEmail
		values["smtp_security"] = ints["smtp_security"]
		values["smtp_verify_peer"] = mt.SMTPVerifyPeer == "1"
		values["smtp_verify_host"] = mt.SMTPVerifyHost == "1"
		values["smtp_authentication"] = ints["smtp_authentication"]
		if ints["smtp_authentication"] == 1 {
			values["username"] = mt.Username
			values["password"] = mt.Passwd
		}
	case mediaTypeScript:
		values["exec_path"] = mt.ExecPath
	case mediaTypeSMS:
		values["gsm_modem"] = mt.GSMModem
	case mediaTypeWebhook:
		values["script"] = mt.Script
		values["timeout"] = mt.Timeout
		values["parameter"] = params
	}
	if err := setFields(d, values); err != nil {
		return diag.FromErr(err)
	}
	return nil
}

func resourceMediaTypeUpdate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	mt := expandMediaType(d)
	mt.MediaTypeID = d.Id()
	if err := client.UpdateMediaType(ctx, mt); err != nil {
		return diag.Errorf("updating media type %s: %s", d.Id(), err)
	}
	return resourceMediaTypeRead(ctx, d, m)
}

func resourceMediaTypeDelete(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	err := deleteError(ctx, client.DeleteMediaType(ctx, d.Id()), func(ctx context.Context) error {
		_, err := client.GetMediaType(ctx, d.Id())
		return err
	})
	if err != nil {
		return diag.Errorf("deleting media type %s: %s", d.Id(), err)
	}
	return nil
}
