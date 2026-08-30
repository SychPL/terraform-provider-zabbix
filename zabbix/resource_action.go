package zabbix

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
)

func resourceAction() *schema.Resource {
	return &schema.Resource{
		Description: "Manages a Zabbix trigger action with \"send message\" operations. " +
			"Actions with recovery or update operations are refused (the provider cannot round-trip them). " +
			"Every attribute is managed authoritatively: after `terraform import`, reproduce the full configuration (all conditions and operations, and non-default `enabled`, `esc_period`, `evaltype`, `pause_suppressed`, `pause_symptoms`, `notify_if_canceled`) and review the plan before the first apply - missing pieces are removed or reset.",
		CreateContext: resourceActionCreate,
		ReadContext:   resourceActionRead,
		UpdateContext: resourceActionUpdate,
		DeleteContext: resourceActionDelete,
		Importer:      passthroughImporter(),
		Timeouts:      defaultTimeouts(),
		CustomizeDiff: resourceActionCustomizeDiff,
		Schema: map[string]*schema.Schema{
			"name": {
				Type:         schema.TypeString,
				Required:     true,
				ValidateFunc: validation.StringIsNotWhiteSpace,
				Description:  "Name of the action. Must be unique in Zabbix.",
			},
			"eventsource": {
				Type:         schema.TypeInt,
				Optional:     true,
				Default:      0,
				ForceNew:     true,
				ValidateFunc: validation.IntInSlice([]int{0}),
				Description:  "Event source. Only 0 (trigger actions) is supported. Changing it forces a new resource.",
			},
			"enabled": {
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     true,
				Description: "Whether the action is enabled.",
			},
			"esc_period": {
				Type:             schema.TypeString,
				Optional:         true,
				Default:          "1h",
				ValidateFunc:     validateEscPeriod,
				DiffSuppressFunc: suppressEquivalentDuration,
				Description:      "Default operation step duration, 60s to 1w, e.g. `1h`.",
			},
			"evaltype": {
				Type:         schema.TypeInt,
				Optional:     true,
				Default:      0,
				ValidateFunc: validation.IntInSlice([]int{0, 1, 2}),
				Description:  "Condition evaluation: 0 - And/Or, 1 - And, 2 - Or. Custom expressions (3) are not supported.",
			},
			"pause_suppressed": {
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     true,
				Description: "Pause escalations for suppressed problems.",
			},
			"pause_symptoms": {
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     true,
				Description: "Pause escalations for symptom problems.",
			},
			"notify_if_canceled": {
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     true,
				Description: "Notify about canceled escalations.",
			},
			"condition": {
				Type:        schema.TypeSet,
				Optional:    true,
				Description: "Conditions filtering the events the action reacts to. Order is not significant.",
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"conditiontype": {
							Type:         schema.TypeInt,
							Required:     true,
							ValidateFunc: validation.IntInSlice([]int{0, 1, 2, 3, 4, 6, 13, 25, 26}),
							Description:  "Condition type: 0 - host group, 1 - host, 2 - trigger, 3 - event name, 4 - trigger severity, 6 - time period, 13 - template, 25 - event tag, 26 - event tag value. (Set verified against Zabbix 6.4.)",
						},
						"operator": {
							Type:        schema.TypeInt,
							Optional:    true,
							Default:     0,
							Description: "Condition operator: 0 - equals, 1 - does not equal, 2 - contains, 3 - does not contain, 4 - in, 5 - >=, 6 - <=, 7 - not in. Allowed values depend on `conditiontype` and are validated at plan time.",
						},
						"value": {
							Type:        schema.TypeString,
							Required:    true,
							Description: "Value to compare with.",
						},
						"value2": {
							Type:        schema.TypeString,
							Optional:    true,
							Default:     "",
							Description: "Second value; the tag name for condition type 26 (event tag value).",
						},
					},
				},
			},
			"operation": {
				Type:        schema.TypeList,
				Required:    true,
				MinItems:    1,
				Description: "Operations executed when the action fires (Zabbix requires at least one). Each operation must have at least one recipient in `user_groups` or `users`. Operations are kept in the configured order.",
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"operationtype": {
							Type:         schema.TypeInt,
							Optional:     true,
							Default:      0,
							ValidateFunc: validation.IntInSlice([]int{0}),
							Description:  "Operation type. Only 0 (send message) is supported.",
						},
						"esc_period": {
							Type:             schema.TypeString,
							Optional:         true,
							Default:          "0",
							ValidateFunc:     validateOperationEscPeriod,
							DiffSuppressFunc: suppressEquivalentDuration,
							Description:      "Step duration; 0 uses the action's `esc_period`.",
						},
						"esc_step_from": {
							Type:         schema.TypeInt,
							Optional:     true,
							Default:      1,
							ValidateFunc: validation.IntAtLeast(1),
							Description:  "First escalation step.",
						},
						"esc_step_to": {
							Type:         schema.TypeInt,
							Optional:     true,
							Default:      1,
							ValidateFunc: validation.IntAtLeast(0),
							Description:  "Last escalation step; 0 means infinite.",
						},
						"mediatypeid": {
							Type:        schema.TypeString,
							Optional:    true,
							Default:     "0",
							Description: "Media type used to send the message; `0` means all media types of the recipients.",
						},
						"default_msg": {
							Type:        schema.TypeBool,
							Optional:    true,
							Default:     true,
							Description: "Use the media type's default message instead of `subject`/`message`.",
						},
						"subject": {
							Type:        schema.TypeString,
							Optional:    true,
							Default:     "",
							Description: "Message subject (used when `default_msg` is false).",
						},
						"message": {
							Type:        schema.TypeString,
							Optional:    true,
							Default:     "",
							Description: "Message body (used when `default_msg` is false).",
						},
						"user_groups": {
							Type:        schema.TypeSet,
							Optional:    true,
							Elem:        &schema.Schema{Type: schema.TypeString, ValidateFunc: validation.StringIsNotWhiteSpace},
							Description: "IDs of user groups to notify.",
						},
						"users": {
							Type:        schema.TypeSet,
							Optional:    true,
							Elem:        &schema.Schema{Type: schema.TypeString, ValidateFunc: validation.StringIsNotWhiteSpace},
							Description: "IDs of users to notify.",
						},
					},
				},
			},
		},
	}
}

// unmanageableHint tells the user how to detach an object the provider refuses
// to manage (Read runs on refresh, so plan and destroy are affected too).
const unmanageableHint = "manage it outside Terraform (terraform state rm <address>) or adjust it in Zabbix so the provider can represent it"

// conditionOperators is the operator matrix accepted by Zabbix 6.4 for trigger
// action conditions, verified empirically against action.create on 6.4.21.
var conditionOperators = map[int][]int{
	0:  {0, 1},       // host group: equals, not equals
	1:  {0, 1},       // host
	2:  {0, 1},       // trigger
	3:  {2, 3},       // event name: contains, not contains
	4:  {0, 1, 5, 6}, // severity: =, !=, >=, <=
	6:  {4, 7},       // time period: in, not in
	13: {0, 1},       // template
	25: {0, 1, 2, 3}, // tag
	26: {0, 1, 2, 3}, // tag value
}

func intIn(n int, list []int) bool {
	for _, v := range list {
		if v == n {
			return true
		}
	}
	return false
}

func resourceActionCustomizeDiff(_ context.Context, d *schema.ResourceDiff, _ interface{}) error {
	if !planKnown(d, "condition", "operation") {
		return nil
	}
	for _, raw := range d.Get("condition").(*schema.Set).List() {
		c := raw.(map[string]interface{})
		// Elements with unknown values carry the SDK's unknown marker instead of
		// typed values; they are validated by Zabbix at apply time.
		ct, ctKnown := c["conditiontype"].(int)
		v2, v2Known := c["value2"].(string)
		if !ctKnown || !v2Known || isUnknownMarker(v2) {
			continue
		}
		if op, ok := c["operator"].(int); ok {
			if allowed, known := conditionOperators[ct]; known && !intIn(op, allowed) {
				return fmt.Errorf("operator %d is not valid for condition type %d (allowed: %v)", op, ct, allowed)
			}
		}
		if v, ok := c["value"].(string); ok && !isUnknownMarker(v) {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("condition value must not be empty or whitespace")
			}
			if ct == 4 {
				if n, err := strconv.Atoi(v); err != nil || n < 0 || n > 5 {
					return fmt.Errorf("condition type 4 (trigger severity) requires a value 0-5, got %q", v)
				}
			}
			if ct == 6 && !userMacroRe.MatchString(v) && !timePeriodRe.MatchString(v) {
				return fmt.Errorf("condition type 6 (time period) requires d-d,hh:mm-hh:mm periods separated by semicolons (or a user macro), got %q", v)
			}
		}
		if ct == 26 && v2 == "" {
			return fmt.Errorf("condition type 26 (event tag value) requires value2 (the tag name)")
		}
		if ct != 26 && v2 != "" {
			return fmt.Errorf("value2 is only supported for condition type 26 (event tag value)")
		}
	}
	for i, raw := range d.Get("operation").([]interface{}) {
		op := raw.(map[string]interface{})
		p := fmt.Sprintf("operation.%d.", i)
		// A block with unknown values (e.g. a user group created in the same
		// plan) is validated by Zabbix at apply time.
		if !planKnown(d, p+"user_groups", p+"users", p+"default_msg", p+"subject", p+"message", p+"esc_step_from", p+"esc_step_to") {
			continue
		}
		if len(setStrings(op["user_groups"]))+len(setStrings(op["users"])) == 0 {
			return fmt.Errorf("operation.%d: at least one recipient in user_groups or users is required", i)
		}
		if op["default_msg"].(bool) && (op["subject"].(string) != "" || op["message"].(string) != "") {
			return fmt.Errorf("operation.%d: subject/message require default_msg = false", i)
		}
		from, to := op["esc_step_from"].(int), op["esc_step_to"].(int)
		if to != 0 && to < from {
			return fmt.Errorf("operation.%d: esc_step_to (%d) must be 0 or >= esc_step_from (%d)", i, to, from)
		}
	}
	return nil
}

// validateActionValues re-runs the plan-time cross-checks on RESOLVED values:
// CustomizeDiff must skip unknown references, so Create and Update validate
// the final data again before mutating (e.g. a default_msg that resolved to
// true next to a static subject must fail, not silently drop the subject).
func validateActionValues(d *schema.ResourceData) error {
	for _, raw := range d.Get("condition").(*schema.Set).List() {
		c := raw.(map[string]interface{})
		ct := c["conditiontype"].(int)
		v := c["value"].(string)
		v2 := c["value2"].(string)
		if op, ok := c["operator"].(int); ok {
			if allowed, known := conditionOperators[ct]; known && !intIn(op, allowed) {
				return fmt.Errorf("operator %d is not valid for condition type %d (allowed: %v)", op, ct, allowed)
			}
		}
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("condition value must not be empty or whitespace")
		}
		if ct == 4 {
			if n, err := strconv.Atoi(v); err != nil || n < 0 || n > 5 {
				return fmt.Errorf("condition type 4 (trigger severity) requires a value 0-5, got %q", v)
			}
		}
		if ct == 6 && !userMacroRe.MatchString(v) && !timePeriodRe.MatchString(v) {
			return fmt.Errorf("condition type 6 (time period) requires d-d,hh:mm-hh:mm periods separated by semicolons (or a user macro), got %q", v)
		}
		if ct == 26 && v2 == "" {
			return fmt.Errorf("condition type 26 (event tag value) requires value2 (the tag name)")
		}
		if ct != 26 && v2 != "" {
			return fmt.Errorf("value2 is only supported for condition type 26 (event tag value)")
		}
	}
	for i, raw := range d.Get("operation").([]interface{}) {
		op := raw.(map[string]interface{})
		if len(setStrings(op["user_groups"]))+len(setStrings(op["users"])) == 0 {
			return fmt.Errorf("operation.%d: at least one recipient in user_groups or users is required", i)
		}
		if op["default_msg"].(bool) && (op["subject"].(string) != "" || op["message"].(string) != "") {
			return fmt.Errorf("operation.%d: subject/message require default_msg = false", i)
		}
		from, to := op["esc_step_from"].(int), op["esc_step_to"].(int)
		if to != 0 && to < from {
			return fmt.Errorf("operation.%d: esc_step_to (%d) must be 0 or >= esc_step_from (%d)", i, to, from)
		}
	}
	return nil
}

func expandAction(d *schema.ResourceData) *Action {
	action := &Action{
		Name:             d.Get("name").(string),
		EventSource:      strconv.Itoa(d.Get("eventsource").(int)),
		Status:           boolToStatus(d.Get("enabled").(bool)),
		EscPeriod:        d.Get("esc_period").(string),
		PauseSuppressed:  boolToFlag(d.Get("pause_suppressed").(bool)),
		PauseSymptoms:    boolToFlag(d.Get("pause_symptoms").(bool)),
		NotifyIfCanceled: boolToFlag(d.Get("notify_if_canceled").(bool)),
		Filter: ActionFilter{
			EvalType:   strconv.Itoa(d.Get("evaltype").(int)),
			Conditions: []ActionCondition{},
		},
		Operations: []ActionOperation{},
	}

	for _, item := range d.Get("condition").(*schema.Set).List() {
		c := item.(map[string]interface{})
		action.Filter.Conditions = append(action.Filter.Conditions, ActionCondition{
			ConditionType: strconv.Itoa(c["conditiontype"].(int)),
			Operator:      strconv.Itoa(c["operator"].(int)),
			Value:         c["value"].(string),
			Value2:        c["value2"].(string),
		})
	}

	for _, item := range d.Get("operation").([]interface{}) {
		o := item.(map[string]interface{})
		op := ActionOperation{
			OperationType: strconv.Itoa(o["operationtype"].(int)),
			EscPeriod:     o["esc_period"].(string),
			EscStepFrom:   strconv.Itoa(o["esc_step_from"].(int)),
			EscStepTo:     strconv.Itoa(o["esc_step_to"].(int)),
			OpMessageGrp:  []ActionOpMessageGrp{},
			OpMessageUsr:  []ActionOpMessageUsr{},
			OpMessage: &ActionOpMessage{
				MediaTypeID: o["mediatypeid"].(string),
				DefaultMsg:  boolToFlag(o["default_msg"].(bool)),
			},
		}
		if !o["default_msg"].(bool) {
			subject, message := o["subject"].(string), o["message"].(string)
			op.OpMessage.Subject, op.OpMessage.Message = &subject, &message
		}
		for _, g := range setStrings(o["user_groups"]) {
			op.OpMessageGrp = append(op.OpMessageGrp, ActionOpMessageGrp{Usrgrpid: g})
		}
		for _, u := range setStrings(o["users"]) {
			op.OpMessageUsr = append(op.OpMessageUsr, ActionOpMessageUsr{UserID: u})
		}
		action.Operations = append(action.Operations, op)
	}
	return action
}

func flattenAction(action *Action) (map[string]interface{}, error) {
	eventsource, err := atoi("eventsource", action.EventSource)
	if err != nil {
		return nil, err
	}
	evaltype, err := atoi("filter.evaltype", action.Filter.EvalType)
	if err != nil {
		return nil, err
	}
	// Refuse to manage what the provider cannot round-trip: action.update
	// replaces filter and operations wholesale, so a custom formula or a
	// non-trigger action would be silently rewritten on the next apply.
	if eventsource != 0 {
		return nil, fmt.Errorf("action has eventsource %d which this provider does not support (only 0, trigger actions); %s", eventsource, unmanageableHint)
	}
	if evaltype == 3 {
		return nil, fmt.Errorf("action uses a custom condition expression (evaltype 3) which this provider does not support; %s", unmanageableHint)
	}
	if len(action.RecoveryOperations) != 0 || len(action.UpdateOperations) != 0 {
		return nil, fmt.Errorf("action has recovery or update operations which this provider does not support; %s", unmanageableHint)
	}

	conds := make([]interface{}, 0, len(action.Filter.Conditions))
	for _, c := range action.Filter.Conditions {
		ct, err := atoi("conditiontype", c.ConditionType)
		if err != nil {
			return nil, err
		}
		op, err := atoi("operator", c.Operator)
		if err != nil {
			return nil, err
		}
		// Refuse combinations the provider cannot write back (action.update
		// replaces the whole filter).
		if allowed, known := conditionOperators[ct]; !known || !intIn(op, allowed) {
			return nil, fmt.Errorf("action uses condition type %d with operator %d which this provider does not support; %s", ct, op, unmanageableHint)
		}
		conds = append(conds, map[string]interface{}{"conditiontype": ct, "operator": op, "value": c.Value, "value2": c.Value2})
	}

	ops := make([]interface{}, 0, len(action.Operations))
	for _, o := range action.Operations {
		opType, err := atoi("operationtype", o.OperationType)
		if err != nil {
			return nil, err
		}
		if opType != 0 {
			// Refuse to manage rather than silently drop the operation on the next
			// update (action.update replaces the whole operations list).
			return nil, fmt.Errorf("action contains an operation of type %d which this provider does not support (only 0, send message); %s", opType, unmanageableHint)
		}
		if len(o.OpConditions) > 0 {
			return nil, fmt.Errorf("action contains an operation with operation conditions (opconditions) which this provider does not support; %s", unmanageableHint)
		}
		from, err := atoi("esc_step_from", o.EscStepFrom)
		if err != nil {
			return nil, err
		}
		to, err := atoi("esc_step_to", o.EscStepTo)
		if err != nil {
			return nil, err
		}
		groups := make([]string, 0, len(o.OpMessageGrp))
		for _, g := range o.OpMessageGrp {
			groups = append(groups, g.Usrgrpid)
		}
		users := make([]string, 0, len(o.OpMessageUsr))
		for _, u := range o.OpMessageUsr {
			users = append(users, u.UserID)
		}
		if o.OpMessage == nil {
			// Every "send message" operation carries an opmessage object; a
			// response without one cannot be represented - refuse, don't guess
			// (a guessed "default message" would overwrite the real one on the
			// next update).
			return nil, fmt.Errorf("action operation has no opmessage object, which this provider cannot represent; %s", unmanageableHint)
		}
		flat := map[string]interface{}{
			"operationtype": opType,
			"esc_period":    o.EscPeriod,
			"esc_step_from": from,
			"esc_step_to":   to,
			"user_groups":   groups,
			"users":         users,
			"mediatypeid":   o.OpMessage.MediaTypeID,
			"default_msg":   o.OpMessage.DefaultMsg == "1",
			"subject":       "",
			"message":       "",
		}
		// Zabbix keeps stale subject/message values when default_msg is
		// switched on; they are meaningless then and not reflected in state.
		if o.OpMessage.DefaultMsg == "0" {
			if o.OpMessage.Subject != nil {
				flat["subject"] = *o.OpMessage.Subject
			}
			if o.OpMessage.Message != nil {
				flat["message"] = *o.OpMessage.Message
			}
		}
		ops = append(ops, flat)
	}

	return map[string]interface{}{
		"name":               action.Name,
		"eventsource":        eventsource,
		"enabled":            action.Status == "0",
		"esc_period":         action.EscPeriod,
		"evaltype":           evaltype,
		"pause_suppressed":   action.PauseSuppressed == "1",
		"pause_symptoms":     action.PauseSymptoms == "1",
		"notify_if_canceled": action.NotifyIfCanceled == "1",
		"condition":          conds,
		"operation":          ops,
	}, nil
}

func resourceActionCreate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	if err := validateActionValues(d); err != nil {
		return diag.FromErr(err)
	}
	id, err := client.CreateAction(ctx, expandAction(d))
	if err != nil {
		return createError("action", d.Get("name").(string), err)
	}
	d.SetId(id)
	return readAfterCreate(ctx, d, m, resourceActionRead, "action")
}

func resourceActionRead(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	action, err := client.GetAction(ctx, d.Id())
	if err != nil {
		return readError(ctx, d, "action", err)
	}
	values, err := flattenAction(action)
	if err != nil {
		return diag.FromErr(err)
	}
	if err := setFields(d, values); err != nil {
		return diag.FromErr(err)
	}
	return nil
}

func resourceActionUpdate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	// Partial mode from the very first statement: SDKv2 would otherwise write
	// the planned values into state even when validation or the preflight read
	// fails before any mutation (see ResourceData.Partial).
	d.Partial(true)
	if err := validateActionValues(d); err != nil {
		return diag.FromErr(err)
	}
	// action.update replaces the filter and operations wholesale: any shape
	// the provider cannot round-trip that appeared outside Terraform since
	// the last refresh would be dropped silently. The full Read mapping
	// decides, so Update refuses exactly the same set as Read.
	current, err := client.GetAction(ctx, d.Id())
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return diag.Errorf("action %s vanished from Zabbix after the plan was created (deleted externally?); re-run terraform apply to refresh and recreate it", d.Id())
		}
		return diag.Errorf("reading action %s: %s", d.Id(), err)
	}
	if _, err := flattenAction(current); err != nil {
		return diag.Errorf("refusing to update action %s: %s", d.Id(), err)
	}
	action := expandAction(d)
	action.ActionID = d.Id()
	if err := client.UpdateAction(ctx, action); err != nil {
		return diag.Errorf("updating action %s: %s", d.Id(), err)
	}
	d.Partial(false)
	return resourceActionRead(ctx, d, m)
}

func resourceActionDelete(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	err := deleteError(ctx, client.DeleteAction(ctx, d.Id()), func(ctx context.Context) error {
		_, err := client.GetAction(ctx, d.Id())
		return err
	})
	if err != nil {
		return diag.Errorf("deleting action %s: %s", d.Id(), err)
	}
	return nil
}
