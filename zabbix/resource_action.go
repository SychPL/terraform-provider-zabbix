package zabbix

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

func resourceAction() *schema.Resource {
	return &schema.Resource{
		CreateContext: resourceActionCreate,
		ReadContext:   resourceActionRead,
		UpdateContext: resourceActionUpdate,
		DeleteContext: resourceActionDelete,
		Schema: map[string]*schema.Schema{
			"name": {
				Type:        schema.TypeString,
				Required:    true,
				Description: "The name of the action",
			},
			"eventsource": {
				Type:        schema.TypeInt,
				Optional:    true,
				Default:     0,
				Description: "Event source: 0 - Triggers",
			},
			"enabled": {
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     true,
				Description: "Whether the action is enabled",
			},
			"esc_period": {
				Type:        schema.TypeString,
				Optional:    true,
				Default:     "1h",
				Description: "Default operation step duration",
			},
			"evaltype": {
				Type:        schema.TypeInt,
				Optional:    true,
				Default:     0,
				Description: "Calculation type: 0 - AND/OR",
			},
			"condition": {
				Type:        schema.TypeList,
				Optional:    true,
				Description: "Conditions to filter events",
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"conditiontype": {
							Type:        schema.TypeInt,
							Required:    true,
							Description: "Condition type, e.g. 16 for Host group, 3 for Trigger name",
						},
						"operator": {
							Type:        schema.TypeInt,
							Optional:    true,
							Default:     0,
							Description: "Operator, e.g. 0 for equals",
						},
						"value": {
							Type:        schema.TypeString,
							Required:    true,
							Description: "Value to compare",
						},
					},
				},
			},
			"operation": {
				Type:        schema.TypeList,
				Optional:    true,
				Description: "Operations to perform when action runs",
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"operationtype": {
							Type:     schema.TypeInt,
							Optional: true,
							Default:  0, // Send message
						},
						"esc_period": {
							Type:     schema.TypeString,
							Optional: true,
							Default:  "0",
						},
						"esc_step_from": {
							Type:     schema.TypeInt,
							Optional: true,
							Default:  1,
						},
						"esc_step_to": {
							Type:     schema.TypeInt,
							Optional: true,
							Default:  1,
						},
						"mediatypeid": {
							Type:     schema.TypeString,
							Optional: true,
							Default:  "",
						},
						"default_msg": {
							Type:     schema.TypeBool,
							Optional: true,
							Default:  true,
						},
						"subject": {
							Type:     schema.TypeString,
							Optional: true,
							Default:  "",
						},
						"message": {
							Type:     schema.TypeString,
							Optional: true,
							Default:  "",
						},
						"user_groups": {
							Type:     schema.TypeSet,
							Optional: true,
							Elem:     &schema.Schema{Type: schema.TypeString},
						},
					},
				},
			},
		},
	}
}

func resourceActionCreate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	action := expandAction(d)
	actionID, err := client.CreateAction(action)
	if err != nil {
		return diag.FromErr(err)
	}

	d.SetId(actionID)
	return resourceActionRead(ctx, d, m)
}

func resourceActionRead(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)
	id := d.Id()

	action, err := client.GetAction(id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[DEBUG ZABBIX] GetAction error: %s\n", err)
		d.SetId("")
		return nil
	}

	d.Set("name", action.Name)
	es, _ := strconv.Atoi(action.EventSource)
	d.Set("eventsource", es)
	d.Set("enabled", action.Status == "0")
	d.Set("esc_period", action.EscPeriod)
	et, _ := strconv.Atoi(action.Filter.EvalType)
	d.Set("evaltype", et)

	// Map conditions back
	conds := make([]map[string]interface{}, len(action.Filter.Conditions))
	for i, c := range action.Filter.Conditions {
		ct, _ := strconv.Atoi(c.ConditionType)
		op, _ := strconv.Atoi(c.Operator)
		conds[i] = map[string]interface{}{
			"conditiontype": ct,
			"operator":      op,
			"value":         c.Value,
		}
	}
	d.Set("condition", conds)

	// Map operations back
	ops := make([]map[string]interface{}, len(action.Operations))
	for i, op := range action.Operations {
		opt, _ := strconv.Atoi(op.OperationType)
		esf, _ := strconv.Atoi(op.EscStepFrom)
		est, _ := strconv.Atoi(op.EscStepTo)
		opMap := map[string]interface{}{
			"operationtype": opt,
			"esc_period":    op.EscPeriod,
			"esc_step_from": esf,
			"esc_step_to":   est,
		}

		if op.OpMessage != nil {
			opMap["mediatypeid"] = op.OpMessage.MediaTypeID
			opMap["default_msg"] = op.OpMessage.DefaultMsg == "1"
			opMap["subject"] = op.OpMessage.Subject
			opMap["message"] = op.OpMessage.Message
		}

		if len(op.OpMessageGrp) > 0 {
			var grpIds []string
			for _, g := range op.OpMessageGrp {
				grpIds = append(grpIds, g.Usrgrpid)
			}
			opMap["user_groups"] = grpIds
		}

		ops[i] = opMap
	}
	d.Set("operation", ops)

	return nil
}

func resourceActionUpdate(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)

	action := expandAction(d)
	action.ActionID = d.Id()

	if err := client.UpdateAction(action); err != nil {
		return diag.FromErr(err)
	}

	return resourceActionRead(ctx, d, m)
}

func resourceActionDelete(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*ZabbixClient)
	id := d.Id()

	if err := client.DeleteAction(id); err != nil {
		return diag.FromErr(err)
	}

	d.SetId("")
	return nil
}

func expandAction(d *schema.ResourceData) *Action {
	status := "0"
	if !d.Get("enabled").(bool) {
		status = "1"
	}

	action := &Action{
		Name:        d.Get("name").(string),
		EventSource: strconv.Itoa(d.Get("eventsource").(int)),
		Status:      status,
		EscPeriod:   d.Get("esc_period").(string),
		Filter: ActionFilter{
			EvalType: strconv.Itoa(d.Get("evaltype").(int)),
		},
	}

	// Expand conditions
	if rawConds, ok := d.GetOk("condition"); ok {
		action.Filter.Conditions = expandActionConditions(rawConds.([]interface{}))
	}

	// Expand operations
	if rawOps, ok := d.GetOk("operation"); ok {
		action.Operations = expandActionOperations(rawOps.([]interface{}))
	}

	return action
}

func expandActionConditions(rawList []interface{}) []ActionCondition {
	conds := make([]ActionCondition, len(rawList))
	for i, item := range rawList {
		m := item.(map[string]interface{})
		conds[i] = ActionCondition{
			ConditionType: strconv.Itoa(m["conditiontype"].(int)),
			Operator:      strconv.Itoa(m["operator"].(int)),
			Value:         m["value"].(string),
		}
	}
	return conds
}

func expandActionOperations(rawList []interface{}) []ActionOperation {
	ops := make([]ActionOperation, len(rawList))
	for i, item := range rawList {
		m := item.(map[string]interface{})
		op := ActionOperation{
			OperationType: strconv.Itoa(m["operationtype"].(int)),
			EscPeriod:     m["esc_period"].(string),
			EscStepFrom:   strconv.Itoa(m["esc_step_from"].(int)),
			EscStepTo:     strconv.Itoa(m["esc_step_to"].(int)),
		}

		// Configure message settings
		defaultMsg := "1"
		if !m["default_msg"].(bool) {
			defaultMsg = "0"
		}

		op.OpMessage = &ActionOpMessage{
			MediaTypeID: m["mediatypeid"].(string),
			DefaultMsg:  defaultMsg,
			Subject:     m["subject"].(string),
			Message:     m["message"].(string),
		}

		// Configure user groups to alert
		if grps, ok := m["user_groups"]; ok {
			grpSet := grps.(*schema.Set)
			var opGrps []ActionOpMessageGrp
			for _, g := range grpSet.List() {
				opGrps = append(opGrps, ActionOpMessageGrp{
					Usrgrpid: g.(string),
				})
			}
			op.OpMessageGrp = opGrps
		}

		ops[i] = op
	}
	return ops
}
