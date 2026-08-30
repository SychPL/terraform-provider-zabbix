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
			"Attributes of other types are rejected at plan time; changing `type` resets the previous type's attributes in Zabbix (including credentials). " +
			"All attributes of the configured type are managed authoritatively: after `terraform import`, reproduce the full configuration (TLS, authentication, credentials, webhook parameters) before the first apply.",
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
		"description": {
			Type:        schema.TypeString,
			Optional:    true,
			Default:     "",
			Description: "Description of the media type.",
		},
		"max_sessions": {
			Type:         schema.TypeInt,
			Optional:     true,
			Default:      1,
			ValidateFunc: validation.IntBetween(0, 100),
			Description:  "Maximum number of alerts processed in parallel: 0 (unlimited) to 100. SMS media types only support 1.",
		},
		"max_attempts": {
			Type:         schema.TypeInt,
			Optional:     true,
			Default:      3,
			ValidateFunc: validation.IntBetween(1, 100),
			Description:  "Maximum number of delivery attempts (1-100).",
		},
		"attempt_interval": {
			Type:        schema.TypeString,
			Optional:    true,
			Default:     "10s",
			Description: "Interval between delivery attempts, 0-1h (e.g. `10s`, `1m`).",
		},
		// Email
		"content_type": {
			Type:         schema.TypeInt,
			Optional:     true,
			Default:      1,
			ValidateFunc: validation.IntInSlice([]int{0, 1}),
			Description:  "Message format: 0 - plain text, 1 - HTML (Email).",
		},
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
		"process_tags": {
			Type:        schema.TypeBool,
			Optional:    true,
			Default:     false,
			Description: "Process the webhook script response as event tags (Webhook).",
		},
		"show_event_menu": {
			Type:        schema.TypeBool,
			Optional:    true,
			Default:     false,
			Description: "Add an entry to the event menu (Webhook). Requires `event_menu_url` and `event_menu_name`.",
		},
		"event_menu_url": {
			Type:        schema.TypeString,
			Optional:    true,
			Default:     "",
			Description: "URL of the event menu entry, supports `{EVENT.TAGS.*}` macros (Webhook).",
		},
		"event_menu_name": {
			Type:        schema.TypeString,
			Optional:    true,
			Default:     "",
			Description: "Name of the event menu entry (Webhook).",
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
	mediaTypeEmail:   {"smtp_server", "smtp_port", "smtp_helo", "smtp_email", "smtp_security", "smtp_verify_peer", "smtp_verify_host", "smtp_authentication", "username", "password", "content_type"},
	mediaTypeScript:  {"exec_path"},
	mediaTypeSMS:     {"gsm_modem"},
	mediaTypeWebhook: {"script", "timeout", "parameter", "process_tags", "show_event_menu", "event_menu_url", "event_menu_name"},
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
	raw := d.GetRawConfig()
	for _, fields := range mediaTypeFields {
		for _, f := range fields {
			if allowed[f] {
				continue
			}
			var set bool
			switch {
			case !known(f):
				set = true // a reference to another resource is an explicit configuration
			case !raw.IsNull():
				// Explicitly written in the configuration, even with the default value.
				v := raw.GetAttr(f)
				set = !v.IsNull() && !(v.Type().IsListType() && v.IsKnown() && v.LengthInt() == 0)
			case f == "parameter":
				set = len(d.Get(f).([]interface{})) > 0
			default:
				set = !reflect.DeepEqual(d.Get(f), mediaTypeSchema[f].Default)
			}
			if set {
				return fmt.Errorf("%s is not supported for media type %d and would be ignored", f, t)
			}
		}
	}

	if known("attempt_interval") {
		if secs, err := parseZabbixDuration(d.Get("attempt_interval").(string)); err != nil || secs < 0 || secs > 3600 {
			return fmt.Errorf("attempt_interval must be a duration between 0 and 1h (e.g. 10s, 1m), got %q", d.Get("attempt_interval"))
		}
	}
	if t == mediaTypeSMS && known("max_sessions") && d.Get("max_sessions").(int) != 1 {
		return fmt.Errorf("max_sessions must be 1 for SMS media types")
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
		switch {
		case known("smtp_authentication") && d.Get("smtp_authentication").(int) == 0:
			for _, f := range []string{"username", "password"} {
				if known(f) && d.Get(f).(string) != "" {
					return fmt.Errorf("username/password require smtp_authentication = 1")
				}
			}
		case known("smtp_authentication"):
			for _, f := range []string{"username", "password"} {
				if err := require(f); err != nil {
					return err
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
		switch {
		case known("show_event_menu") && d.Get("show_event_menu").(bool):
			for _, f := range []string{"event_menu_url", "event_menu_name"} {
				if err := require(f); err != nil {
					return err
				}
			}
		case known("show_event_menu"):
			for _, f := range []string{"event_menu_url", "event_menu_name"} {
				if known(f) && d.Get(f).(string) != "" {
					return fmt.Errorf("%s requires show_event_menu = true", f)
				}
			}
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
		Description:        d.Get("description").(string),
		MaxSessions:        strconv.Itoa(d.Get("max_sessions").(int)),
		MaxAttempts:        strconv.Itoa(d.Get("max_attempts").(int)),
		AttemptInterval:    d.Get("attempt_interval").(string),
		ContentType:        strconv.Itoa(d.Get("content_type").(int)),
		ProcessTags:        boolToFlag(d.Get("process_tags").(bool)),
		ShowEventMenu:      boolToFlag(d.Get("show_event_menu").(bool)),
		EventMenuURL:       d.Get("event_menu_url").(string),
		EventMenuName:      d.Get("event_menu_name").(string),
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

	// The type decides everything else, so it is validated first; numeric
	// fields are parsed only for the type they belong to, tolerating empty
	// values (objects created outside the provider).
	mtType, err := atoi("type", mt.Type)
	if err != nil {
		return diag.Errorf("media type %s: %s; %s", d.Id(), err, unmanageableHint)
	}
	if _, ok := mediaTypeFields[mtType]; !ok {
		return diag.Errorf("media type %s has type %d which this provider does not support; %s", d.Id(), mtType, unmanageableHint)
	}
	if mtType == mediaTypeScript && len(mt.Parameters) > 0 {
		return diag.Errorf("media type %s is a script media type with parameters, which this provider does not support; %s", d.Id(), unmanageableHint)
	}
	// Numeric defaults come from the schema (single source); empty API values
	// (objects created outside the provider) keep the default.
	ints := map[string]int{"type": mtType}
	for _, f := range []string{"smtp_port", "smtp_security", "smtp_authentication", "content_type", "max_sessions", "max_attempts"} {
		ints[f] = mediaTypeSchema[f].Default.(int)
	}
	for field, raw := range map[string]string{"max_sessions": mt.MaxSessions, "max_attempts": mt.MaxAttempts} {
		if raw == "" {
			continue
		}
		n, err := atoi(field, raw)
		if err != nil {
			return diag.Errorf("media type %s: %s; %s", d.Id(), err, unmanageableHint)
		}
		ints[field] = n
	}
	if mtType == mediaTypeEmail {
		for field, raw := range map[string]string{
			"smtp_port": mt.SMTPPort, "smtp_security": mt.SMTPSecurity, "smtp_authentication": mt.SMTPAuthentication, "content_type": mt.ContentType,
		} {
			if raw == "" {
				continue // keep the schema default
			}
			n, err := atoi(field, raw)
			if err != nil {
				return diag.Errorf("media type %s: %s; %s", d.Id(), err, unmanageableHint)
			}
			ints[field] = n
		}
	}
	params := make([]map[string]interface{}, len(mt.Parameters))
	for i, p := range mt.Parameters {
		params[i] = map[string]interface{}{"name": p.Name, "value": p.Value}
	}

	// Only attributes relevant for the type are taken from the API; the rest
	// are reset to their schema defaults (the schema is the single source of
	// those values) so that stale values left in Zabbix after a type change do
	// not produce a permanent diff.
	values := map[string]interface{}{}
	for _, fields := range mediaTypeFields {
		for _, f := range fields {
			if f == "parameter" {
				values[f] = []map[string]interface{}{}
				continue
			}
			values[f] = mediaTypeSchema[f].Default
		}
	}
	attemptInterval := mediaTypeSchema["attempt_interval"].Default.(string)
	if mt.AttemptInterval != "" {
		attemptInterval = mt.AttemptInterval
	}
	values["name"] = mt.Name
	values["type"] = ints["type"]
	values["enabled"] = mt.Status == "0"
	values["description"] = mt.Description
	values["max_sessions"] = ints["max_sessions"]
	values["max_attempts"] = ints["max_attempts"]
	values["attempt_interval"] = attemptInterval
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
		values["content_type"] = ints["content_type"]
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
		values["process_tags"] = mt.ProcessTags == "1"
		values["show_event_menu"] = mt.ShowEventMenu == "1"
		values["event_menu_url"] = mt.EventMenuURL
		values["event_menu_name"] = mt.EventMenuName
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
	mt.ClearParameters = d.HasChange("type")
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
