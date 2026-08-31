package zabbix

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

// fixtureServer answers every *.get call with the given JSON result.
func fixtureServer(t *testing.T, method string, result string) *ZabbixClient {
	t.Helper()
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		if req.Method != method {
			t.Errorf("unexpected method %s", req.Method)
			return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
		}
		return json.RawMessage(result), nil
	})
	return newTestClient(t, s, ClientConfig{APIToken: "t"})
}

func readInto(t *testing.T, r *schema.Resource, c *ZabbixClient, id string) *schema.ResourceData {
	t.Helper()
	d := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{})
	d.SetId(id)
	if diags := r.ReadContext(context.Background(), d, c); diags.HasError() {
		t.Fatalf("read: %v", diags)
	}
	return d
}

func TestHostRead_PicksMainAgentInterface(t *testing.T) {
	// Real host.get shape: SNMP interface first, agent (DNS mode) second.
	c := fixtureServer(t, "host.get", `[{
		"hostid":"10633","host":"exp-h","name":"Visible","status":"1","description":"d",
		"parentTemplates":[{"templateid":"10001"}],
		"hostgroups":[{"groupid":"25"},{"groupid":"26"}],
		"interfaces":[
			{"interfaceid":"33","type":"2","main":"1","useip":"1","ip":"10.0.0.2","dns":"","port":"161"},
			{"interfaceid":"34","type":"1","main":"1","useip":"0","ip":"","dns":"h.local","port":"10050"}
		]}]`)
	d := readInto(t, resourceHost(), c, "10633")

	want := map[string]interface{}{
		"host": "exp-h", "name": "Visible", "enabled": false, "description": "d",
		"use_ip": false, "ip": "", "dns": "h.local", "port": "10050",
	}
	for k, v := range want {
		if got := d.Get(k); got != v {
			t.Errorf("%s: want %v, got %v", k, v, got)
		}
	}
	if d.Get("groups").(*schema.Set).Len() != 2 || d.Get("templates").(*schema.Set).Len() != 1 {
		t.Errorf("groups/templates not mapped: %v / %v", d.Get("groups"), d.Get("templates"))
	}
}

func TestHostRead_NoAgentInterfaceShowsDrift(t *testing.T) {
	// The agent interface was removed outside Terraform: state must reflect it
	// so that the plan shows the drift (and the next apply recreates it).
	c := fixtureServer(t, "host.get", `[{"hostid":"1","host":"snmp-only","name":"snmp-only","status":"0","description":"",
		"parentTemplates":[],"hostgroups":[{"groupid":"2"}],
		"interfaces":[{"interfaceid":"5","type":"2","main":"1","useip":"1","ip":"10.0.0.2","dns":"","port":"161"}]}]`)
	d := schema.TestResourceDataRaw(t, resourceHost().Schema, map[string]interface{}{"ip": "192.0.2.1", "port": "10051"})
	d.SetId("1")
	if diags := resourceHost().ReadContext(context.Background(), d, c); diags.HasError() {
		t.Fatal(diags)
	}
	if d.Get("ip") != "" || d.Get("port") != "10050" || d.Get("use_ip") != true {
		t.Errorf("a missing agent interface must be reflected in state, got ip=%v port=%v", d.Get("ip"), d.Get("port"))
	}
}

func TestHostRead_IgnoresNonMainAgentInterface(t *testing.T) {
	// An agent interface with main=0 is not the managed interface: the host
	// reads as agentless (visible drift) and the secondary interface is left
	// untouched.
	c := fixtureServer(t, "host.get", `[{"hostid":"1","host":"h","name":"h","status":"0","flags":"0","description":"",
		"parentTemplates":[],"hostgroups":[{"groupid":"2"}],
		"interfaces":[{"interfaceid":"9","type":"1","main":"0","useip":"1","ip":"10.0.0.9","dns":"","port":"10060"}]}]`)
	d := readInto(t, resourceHost(), c, "1")
	if d.Get("ip") != "" || d.Get("port") != defaultAgentPort || d.Get("use_ip") != true {
		t.Errorf("non-main agent interface must not be adopted, got ip=%v port=%v", d.Get("ip"), d.Get("port"))
	}
}

func TestHostRead_TransportErrorKeepsID(t *testing.T) {
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		return nil, &JsonRpcError{Code: -32500, Message: "Application error.", Data: "database down"}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	d := schema.TestResourceDataRaw(t, resourceHost().Schema, map[string]interface{}{})
	d.SetId("7")
	diags := resourceHost().ReadContext(context.Background(), d, c)
	if !diags.HasError() || d.Id() != "7" {
		t.Fatalf("API error must be surfaced and keep the ID; diags=%v id=%q", diags, d.Id())
	}
}

func TestMediaTypeRead_TypeAwareReset(t *testing.T) {
	// Webhook whose row still carries stale SMTP values from a previous type.
	c := fixtureServer(t, "mediatype.get", `[{"mediatypeid":"46","type":"4","name":"wh","status":"0",
		"smtp_server":"stale.mail","smtp_port":"587","smtp_helo":"x","smtp_email":"a@x","smtp_security":"1",
		"smtp_verify_peer":"1","smtp_verify_host":"1","smtp_authentication":"1","username":"u","passwd":"secret",
		"exec_path":"","gsm_modem":"","script":"return 1;","timeout":"10s",
		"description":"managed","maxsessions":"0","maxattempts":"5","attempt_interval":"1m",
		"content_type":"0","process_tags":"1","show_event_menu":"1",
		"event_menu_url":"https://x/{EVENT.ID}","event_menu_name":"Open",
		"parameters":[{"name":"a","value":"b"},{"name":"c","value":"d"}]}]`)
	d := readInto(t, resourceMediaType(), c, "46")

	if d.Get("smtp_server") != "" || d.Get("smtp_port") != 25 || d.Get("password") != "" || d.Get("smtp_authentication") != 0 {
		t.Errorf("email attributes must be reset for a webhook: %v %v %v", d.Get("smtp_server"), d.Get("smtp_port"), d.Get("password"))
	}
	if d.Get("script") != "return 1;" || d.Get("timeout") != "10s" {
		t.Errorf("webhook attributes not mapped")
	}
	if d.Get("content_type") != 1 || d.Get("email_provider") != 0 {
		t.Errorf("email-only fields must be reset to their schema defaults for a webhook, got content_type=%v email_provider=%v", d.Get("content_type"), d.Get("email_provider"))
	}
	if d.Get("description") != "managed" || d.Get("max_sessions") != 0 || d.Get("max_attempts") != 5 || d.Get("attempt_interval") != "1m" {
		t.Errorf("common fields not mapped: %v %v %v %v", d.Get("description"), d.Get("max_sessions"), d.Get("max_attempts"), d.Get("attempt_interval"))
	}
	if d.Get("process_tags") != true || d.Get("show_event_menu") != true || d.Get("event_menu_url") != "https://x/{EVENT.ID}" || d.Get("event_menu_name") != "Open" {
		t.Errorf("webhook event menu fields not mapped")
	}
	params := d.Get("parameter").([]interface{})
	if len(params) != 2 || params[1].(map[string]interface{})["value"] != "d" {
		t.Errorf("parameters not mapped: %v", params)
	}
}

func TestMediaTypeRead_EmailWithAuth(t *testing.T) {
	c := fixtureServer(t, "mediatype.get", `[{"mediatypeid":"45","type":"0","name":"mail","status":"1",
		"smtp_server":"mail.x","smtp_port":"587","smtp_helo":"x","smtp_email":"a@x","smtp_security":"1",
		"smtp_verify_peer":"1","smtp_verify_host":"0","smtp_authentication":"1","username":"u","passwd":"secret",
		"exec_path":"","gsm_modem":"","script":"","timeout":"30s","content_type":"0","provider":"3","parameters":[]}]`)
	d := readInto(t, resourceMediaType(), c, "45")
	want := map[string]interface{}{
		"enabled": false, "smtp_server": "mail.x", "smtp_port": 587, "smtp_security": 1,
		"smtp_verify_peer": true, "smtp_verify_host": false, "smtp_authentication": 1,
		"username": "u", "password": "secret", "script": "", "timeout": "30s",
		"content_type": 0, "email_provider": 3, "max_attempts": 3, "attempt_interval": "10s",
	}
	for k, v := range want {
		if got := d.Get(k); got != v {
			t.Errorf("%s: want %v, got %v", k, v, got)
		}
	}
	if len(d.Get("parameter").([]interface{})) != 0 {
		t.Error("parameter must be an empty list")
	}
}

const actionFixture = `[{"actionid":"10","name":"exp-a","eventsource":"0","status":"0","esc_period":"1h",
	"pause_suppressed":"1","pause_symptoms":"0","notify_if_canceled":"0",
	"filter":{"evaltype":"2","formula":"","conditions":[
		{"conditiontype":"0","operator":"0","value":"25","value2":"","formulaid":"A"},
		{"conditiontype":"26","operator":"2","value":"prod","value2":"env","formulaid":"B"}],"eval_formula":"A or B"},
	"operations":[{"operationid":"14","actionid":"10","operationtype":"%OP%","esc_period":"0","esc_step_from":"1","esc_step_to":"0",
		"evaltype":"0","opconditions":[],
		"opmessage":{"default_msg":"%DM%","subject":"S","message":"M","mediatypeid":"46"},
		"opmessage_grp":[{"usrgrpid":"7"}],"opmessage_usr":[{"userid":"1"},{"userid":"3"}]}]}]`

func TestActionRead_Mapping(t *testing.T) {
	c := fixtureServer(t, "action.get", strings.NewReplacer("%OP%", "0", "%DM%", "0").Replace(actionFixture))
	d := readInto(t, resourceAction(), c, "10")

	if d.Get("evaltype") != 2 || d.Get("pause_suppressed") != true || d.Get("pause_symptoms") != false || d.Get("notify_if_canceled") != false {
		t.Errorf("action attributes not mapped")
	}
	conds := d.Get("condition").(*schema.Set).List()
	if len(conds) != 2 {
		t.Fatalf("want 2 conditions, got %d", len(conds))
	}
	var tagCond map[string]interface{}
	for _, raw := range conds {
		if m := raw.(map[string]interface{}); m["conditiontype"] == 26 {
			tagCond = m
		}
	}
	if tagCond == nil || tagCond["value"] != "prod" || tagCond["value2"] != "env" || tagCond["operator"] != 2 {
		t.Errorf("event tag value condition not mapped: %v", tagCond)
	}

	op := d.Get("operation").([]interface{})[0].(map[string]interface{})
	// default_msg "0" in fixture -> subject/message reflected
	if op["default_msg"] != false || op["subject"] != "S" || op["message"] != "M" || op["mediatypeid"] != "46" || op["esc_step_to"] != 0 {
		t.Errorf("operation not mapped: %v", op)
	}
	if op["user_groups"].(*schema.Set).Len() != 1 || op["users"].(*schema.Set).Len() != 2 {
		t.Errorf("recipients not mapped: %v", op)
	}
}

func TestActionRead_DefaultMsgHidesStaleSubject(t *testing.T) {
	c := fixtureServer(t, "action.get", strings.NewReplacer("%OP%", "0", "%DM%", "1").Replace(actionFixture))
	d := readInto(t, resourceAction(), c, "10")
	op := d.Get("operation").([]interface{})[0].(map[string]interface{})
	if op["default_msg"] != true || op["subject"] != "" || op["message"] != "" {
		t.Errorf("stale subject/message must not be reflected when default_msg is on: %v", op)
	}
}

func TestActionRead_RefusesUnsupportedEventSourceAndEvaltype(t *testing.T) {
	base := strings.NewReplacer("%OP%", "0", "%DM%", "1").Replace(actionFixture)
	cases := map[string]string{
		"eventsource": strings.Replace(base, `"eventsource":"0"`, `"eventsource":"1"`, 1),
		"evaltype":    strings.Replace(base, `"evaltype":"2"`, `"evaltype":"3"`, 1),
	}
	for name, fixture := range cases {
		t.Run(name, func(t *testing.T) {
			c := fixtureServer(t, "action.get", fixture)
			d := schema.TestResourceDataRaw(t, resourceAction().Schema, map[string]interface{}{})
			d.SetId("10")
			diags := resourceAction().ReadContext(context.Background(), d, c)
			if !diags.HasError() || !strings.Contains(diags[0].Summary, "does not support") || d.Id() != "10" {
				t.Fatalf("must refuse to manage and keep the ID, got %v id=%q", diags, d.Id())
			}
		})
	}
}

func TestActionRead_RefusesUnknownConditionOperator(t *testing.T) {
	// Operator 8 (matches) exists in newer Zabbix versions but is rejected by
	// 6.4; an imported action carrying it cannot be round-tripped.
	fixture := strings.Replace(strings.NewReplacer("%OP%", "0", "%DM%", "1").Replace(actionFixture),
		`{"conditiontype":"26","operator":"2","value":"prod","value2":"env","formulaid":"B"}`,
		`{"conditiontype":"26","operator":"8","value":"prod","value2":"env","formulaid":"B"}`, 1)
	c := fixtureServer(t, "action.get", fixture)
	d := schema.TestResourceDataRaw(t, resourceAction().Schema, map[string]interface{}{})
	d.SetId("10")
	diags := resourceAction().ReadContext(context.Background(), d, c)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "operator 8") || !strings.Contains(diags[0].Summary, "terraform state rm") {
		t.Fatalf("an unsupported condition operator must be refused with a hint, got %v", diags)
	}
}

func TestActionRead_RefusesRecoveryAndUpdateOperations(t *testing.T) {
	base := strings.NewReplacer("%OP%", "0", "%DM%", "1").Replace(actionFixture)
	for _, field := range []string{"recovery_operations", "update_operations"} {
		fixture := strings.Replace(base, `"operations":[`, `"`+field+`":[{"operationid":"99"}],"operations":[`, 1)
		c := fixtureServer(t, "action.get", fixture)
		d := schema.TestResourceDataRaw(t, resourceAction().Schema, map[string]interface{}{})
		d.SetId("10")
		diags := resourceAction().ReadContext(context.Background(), d, c)
		if !diags.HasError() || !strings.Contains(diags[0].Summary, "recovery or update operations") || !strings.Contains(diags[0].Summary, "terraform state rm") {
			t.Fatalf("%s must be refused with a state rm hint, got %v", field, diags)
		}
	}
}

func TestActionRead_RefusesOperationWithoutOpMessage(t *testing.T) {
	base := strings.NewReplacer("%OP%", "0", "%DM%", "1").Replace(actionFixture)
	fixture := strings.Replace(base, `"opmessage":{"default_msg":"1","subject":"S","message":"M","mediatypeid":"46"},`, ``, 1)
	c := fixtureServer(t, "action.get", fixture)
	d := schema.TestResourceDataRaw(t, resourceAction().Schema, map[string]interface{}{})
	d.SetId("10")
	diags := resourceAction().ReadContext(context.Background(), d, c)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "no opmessage") || !strings.Contains(diags[0].Summary, "terraform state rm") {
		t.Fatalf("an operation without opmessage must be refused, got %v", diags)
	}
}

func TestActionRead_RefusesOperationConditions(t *testing.T) {
	fixture := strings.Replace(strings.NewReplacer("%OP%", "0", "%DM%", "1").Replace(actionFixture),
		`"opconditions":[]`, `"opconditions":[{"conditiontype":"14","operator":"0","value":"0"}]`, 1)
	c := fixtureServer(t, "action.get", fixture)
	d := schema.TestResourceDataRaw(t, resourceAction().Schema, map[string]interface{}{})
	d.SetId("10")
	diags := resourceAction().ReadContext(context.Background(), d, c)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "opconditions") || !strings.Contains(diags[0].Summary, "terraform state rm") {
		t.Fatalf("operation conditions must be refused with a state rm hint, got %v", diags)
	}
}

func TestMediaTypeRead_RefusesRestrictedResponse(t *testing.T) {
	// Since 6.4.19 non-Super-Admin roles get only mediatypeid, name, type,
	// status and maxattempts; adopting that would zero out the real
	// configuration. The read must fail and leave EVERY existing attribute
	// untouched - including the stored credentials.
	c := fixtureServer(t, "mediatype.get", `[{"mediatypeid":"9","name":"m","type":"0","status":"0","maxattempts":"3"}]`)
	old := map[string]string{
		"name": "mail", "type": "0", "enabled": "true",
		"smtp_server": "keep.mail", "smtp_port": "587", "smtp_helo": "h", "smtp_email": "e@x",
		"smtp_security": "0", "smtp_verify_peer": "false", "smtp_verify_host": "false",
		"smtp_authentication": "1", "username": "u", "password": "keep-me",
		"exec_path": "", "gsm_modem": "", "script": "", "timeout": "30s",
		"description": "", "max_sessions": "1", "max_attempts": "3", "attempt_interval": "10s",
		"content_type": "1", "process_tags": "false", "show_event_menu": "false",
		"event_menu_url": "", "event_menu_name": "", "parameter.#": "0",
	}
	r := resourceMediaType()
	d := r.Data(&terraform.InstanceState{ID: "9", Attributes: old})
	diags := r.ReadContext(context.Background(), d, c)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "Super Admin") || d.Id() != "9" {
		t.Fatalf("a restricted mediatype.get response must be refused and keep the ID, got %v id=%q", diags, d.Id())
	}
	st := d.State()
	if st == nil {
		t.Fatal("the state must survive a refused read, got nil")
	}
	for k, v := range old {
		if got := st.Attributes[k]; got != v {
			t.Fatalf("attribute %s must keep %q after a refused read, got %q", k, v, got)
		}
	}
}

func TestMediaTypeRead_RefusesOutOfRangeWebhookTimeout(t *testing.T) {
	// d.Set bypasses schema validators: adopting an out-of-range timeout from
	// the API would poison the state, so the read must be fail-closed.
	c := fixtureServer(t, "mediatype.get", `[{"mediatypeid":"9","type":"4","name":"wh","status":"0",
		"smtp_server":"","smtp_port":"25","smtp_helo":"","smtp_email":"","smtp_security":"0","smtp_verify_peer":"0","smtp_verify_host":"0",
		"smtp_authentication":"0","username":"","passwd":"","exec_path":"","gsm_modem":"","script":"return 1;","timeout":"5m","parameters":[]}]`)
	d := schema.TestResourceDataRaw(t, resourceMediaType().Schema, map[string]interface{}{})
	d.SetId("9")
	diags := resourceMediaType().ReadContext(context.Background(), d, c)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "1-60s") || !strings.Contains(diags[0].Summary, "terraform state rm") || d.Id() != "9" {
		t.Fatalf("an out-of-range webhook timeout must be refused with the ID kept, got %v id=%q", diags, d.Id())
	}
}

func TestMediaTypeRead_RefusesUnsupportedType(t *testing.T) {
	c := fixtureServer(t, "mediatype.get", `[{"mediatypeid":"9","type":"3","name":"jabber","status":"0",
		"smtp_server":"","smtp_port":"25","smtp_helo":"","smtp_email":"","smtp_security":"0","smtp_verify_peer":"0","smtp_verify_host":"0",
		"smtp_authentication":"0","username":"","passwd":"","exec_path":"","gsm_modem":"","script":"","timeout":"30s","parameters":[]}]`)
	d := schema.TestResourceDataRaw(t, resourceMediaType().Schema, map[string]interface{}{})
	d.SetId("9")
	diags := resourceMediaType().ReadContext(context.Background(), d, c)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "does not support") || !strings.Contains(diags[0].Summary, "terraform state rm") {
		t.Fatalf("unsupported media type type must be refused with a hint, got %v", diags)
	}
}

func TestMediaTypeRead_ForeignNumericFieldsAreTolerated(t *testing.T) {
	// A webhook whose row carries an empty/garbage smtp_port (created outside
	// the provider) must read fine: smtp_port belongs to the email type only.
	c := fixtureServer(t, "mediatype.get", `[{"mediatypeid":"9","type":"4","name":"wh","status":"0",
		"smtp_server":"","smtp_port":"twenty-five","smtp_helo":"","smtp_email":"","smtp_security":"","smtp_verify_peer":"0","smtp_verify_host":"0",
		"smtp_authentication":"0","username":"","passwd":"","exec_path":"","gsm_modem":"","script":"return 1;","timeout":"30s","parameters":[]}]`)
	d := readInto(t, resourceMediaType(), c, "9")
	if d.Get("smtp_port") != 25 || d.Get("script") != "return 1;" {
		t.Fatalf("foreign numeric fields must fall back to defaults, got smtp_port=%v", d.Get("smtp_port"))
	}
}

func TestMediaTypeRead_OwnNonNumericFieldRefusedWithHint(t *testing.T) {
	c := fixtureServer(t, "mediatype.get", `[{"mediatypeid":"9","type":"0","name":"mail","status":"0",
		"smtp_server":"mail.x","smtp_port":"twenty-five","smtp_helo":"x","smtp_email":"a@x","smtp_security":"0","smtp_verify_peer":"0","smtp_verify_host":"0",
		"smtp_authentication":"0","username":"","passwd":"","exec_path":"","gsm_modem":"","script":"","timeout":"30s","parameters":[]}]`)
	d := schema.TestResourceDataRaw(t, resourceMediaType().Schema, map[string]interface{}{})
	d.SetId("9")
	diags := resourceMediaType().ReadContext(context.Background(), d, c)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "smtp_port") || !strings.Contains(diags[0].Summary, "terraform state rm") || d.Id() != "9" {
		t.Fatalf("an unparsable own-type field must be refused with a hint and keep the ID, got %v id=%q", diags, d.Id())
	}
}

func TestHostResource_NoInterfaceLifecycle(t *testing.T) {
	// Create without any interface, then add one, then remove it again.
	hostFixture := `[{"hostid":"1","host":"h","name":"h","status":"0","flags":"0","description":"",
		"parentTemplates":[],"hostgroups":[{"groupid":"2"}],"interfaces":[%s]}]`
	agent := `{"interfaceid":"5","type":"1","main":"1","useip":"1","ip":"192.0.2.1","dns":"","port":"10050"}`
	current := ""
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "host.create":
			var params map[string]interface{}
			_ = json.Unmarshal(req.Params, &params)
			if _, ok := params["interfaces"]; ok {
				t.Errorf("host.create without an address must not send interfaces: %s", req.Params)
			}
			return map[string][]string{"hostids": {"1"}}, nil
		case "host.update":
			return map[string][]string{"hostids": {"1"}}, nil
		case "hostinterface.create":
			current = agent
			return map[string][]string{"interfaceids": {"5"}}, nil
		case "hostinterface.delete":
			current = ""
			return map[string][]string{"interfaceids": {"5"}}, nil
		case "host.get":
			return json.RawMessage(fmt.Sprintf(hostFixture, current)), nil
		}
		t.Errorf("unexpected method %s", req.Method)
		return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	r := resourceHost()

	d := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{"host": "h", "groups": []interface{}{"2"}})
	if diags := r.CreateContext(context.Background(), d, c); diags.HasError() {
		t.Fatal(diags)
	}
	if d.Get("ip") != "" || d.Get("dns") != "" {
		t.Fatalf("no-interface host must read empty address, got ip=%v dns=%v", d.Get("ip"), d.Get("dns"))
	}

	// Add an address -> the agent interface is created.
	d = schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{"host": "h", "groups": []interface{}{"2"}, "ip": "192.0.2.1"})
	d.SetId("1")
	if diags := r.UpdateContext(context.Background(), d, c); diags.HasError() {
		t.Fatal(diags)
	}
	if len(s.calls("hostinterface.create")) != 1 || d.Get("ip") != "192.0.2.1" {
		t.Fatalf("adding an address must create the interface, ip=%v", d.Get("ip"))
	}

	// Remove the address -> the agent interface is deleted.
	d = schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{"host": "h", "groups": []interface{}{"2"}})
	d.SetId("1")
	if diags := r.UpdateContext(context.Background(), d, c); diags.HasError() {
		t.Fatal(diags)
	}
	if len(s.calls("hostinterface.delete")) != 1 || d.Get("ip") != "" {
		t.Fatalf("removing the address must delete the interface, ip=%v", d.Get("ip"))
	}
}

func TestHostCreate_SendsInterfacePayload(t *testing.T) {
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "host.create":
			var params struct {
				Interfaces []map[string]string `json:"interfaces"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				t.Errorf("unmarshal host.create params: %v", err)
				return nil, &JsonRpcError{Code: -32602, Message: "Invalid params."}
			}
			want := map[string]string{"type": "1", "main": "1", "useip": "1", "ip": "192.0.2.7", "dns": "", "port": "10051"}
			if len(params.Interfaces) != 1 || !reflect.DeepEqual(params.Interfaces[0], want) {
				t.Errorf("interface payload: want %v, got %v", want, params.Interfaces)
			}
			return map[string][]string{"hostids": {"1"}}, nil
		case "host.get":
			return json.RawMessage(`[{"hostid":"1","host":"h","name":"h","status":"0","flags":"0","description":"",
				"parentTemplates":[],"hostgroups":[{"groupid":"2"}],
				"interfaces":[{"interfaceid":"5","type":"1","main":"1","useip":"1","ip":"192.0.2.7","dns":"","port":"10051"}]}]`), nil
		}
		t.Errorf("unexpected method %s", req.Method)
		return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	r := resourceHost()
	d := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{"host": "h", "groups": []interface{}{"2"}, "ip": "192.0.2.7", "port": "10051"})
	if diags := r.CreateContext(context.Background(), d, c); diags.HasError() {
		t.Fatal(diags)
	}
	if d.Get("port") != "10051" || d.Get("ip") != "192.0.2.7" {
		t.Errorf("round-trip: ip=%v port=%v", d.Get("ip"), d.Get("port"))
	}
}

func TestActionParams_CustomSubjectAlwaysSent(t *testing.T) {
	r := resourceAction()
	d := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{
		"name": "a",
		"operation": []interface{}{map[string]interface{}{
			"users": []interface{}{"1"}, "default_msg": false, "subject": "", "message": "",
		}},
	})
	b, err := json.Marshal(actionParams(expandAction(d)))
	if err != nil {
		t.Fatal(err)
	}
	// An empty subject/message must be transmitted: action.update merges
	// omitted fields with the stored values, which would resurrect a stale
	// subject and produce a perpetual diff.
	if !strings.Contains(string(b), `"subject":""`) || !strings.Contains(string(b), `"message":""`) {
		t.Errorf("custom message must always send subject/message, got %s", b)
	}

	d = schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{
		"name": "a",
		"operation": []interface{}{map[string]interface{}{
			"users": []interface{}{"1"}, "subject": "", "message": "",
		}},
	})
	b, err = json.Marshal(actionParams(expandAction(d)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"subject"`) || strings.Contains(string(b), `"message":`) {
		t.Errorf("default_msg = true must not send subject/message (the API rejects them), got %s", b)
	}
}

func TestHostApplyValidation_RejectsInvalidResolvedValues(t *testing.T) {
	// CustomizeDiff skips unknown references; the same cross-checks must run
	// again on the resolved values before any mutation.
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		t.Errorf("no API call expected, got %s", req.Method)
		return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	r := resourceHost()
	d := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{"host": "h", "groups": []interface{}{"2"}, "use_ip": false})
	if diags := r.CreateContext(context.Background(), d, c); !diags.HasError() || !strings.Contains(diags[0].Summary, "dns is required") {
		t.Fatalf("an invalid resolved address must fail before create, got %v", diags)
	}
	d = schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{"host": "h", "groups": []interface{}{"2"}, "use_ip": false})
	d.SetId("1")
	if diags := r.UpdateContext(context.Background(), d, c); !diags.HasError() || !strings.Contains(diags[0].Summary, "dns is required") {
		t.Fatalf("an invalid resolved address must fail before update, got %v", diags)
	}
	// Formats that resolved to garbage must also fail before any mutation.
	dIP := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{"host": "h", "groups": []interface{}{"2"}, "ip": "agent.example.test"})
	if diags := r.CreateContext(context.Background(), dIP, c); !diags.HasError() || !strings.Contains(diags[0].Summary, "not a valid IP") {
		t.Fatalf("a DNS name that resolved into ip must fail before create, got %v", diags)
	}
	dPort := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{"host": "h", "groups": []interface{}{"2"}, "ip": "192.0.2.7", "port": "70000"})
	dPort.SetId("1")
	if diags := r.UpdateContext(context.Background(), dPort, c); !diags.HasError() || !strings.Contains(diags[0].Summary, "port number 1-65535") {
		t.Fatalf("a resolved out-of-range port must fail before update, got %v", diags)
	}
	// Partial mode is on from the first statement: a failure before the API
	// must not persist the planned values either.
	if st := d.State(); st != nil && st.Attributes["use_ip"] == "false" {
		t.Fatalf("planned values must not reach the state when validation fails before the API, got %+v", st.Attributes)
	}
}

func TestActionApplyValidation_RejectsResolvedConflicts(t *testing.T) {
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		t.Errorf("no API call expected, got %s", req.Method)
		return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	r := resourceAction()
	raw := map[string]interface{}{"name": "a",
		"operation": []interface{}{map[string]interface{}{"users": []interface{}{"1"}, "default_msg": true, "subject": "S"}}}
	if diags := r.CreateContext(context.Background(), schema.TestResourceDataRaw(t, r.Schema, raw), c); !diags.HasError() || !strings.Contains(diags[0].Summary, "default_msg = false") {
		t.Fatalf("a default_msg that resolved to true next to a subject must fail before create, got %v", diags)
	}
	d := schema.TestResourceDataRaw(t, r.Schema, raw)
	d.SetId("10")
	if diags := r.UpdateContext(context.Background(), d, c); !diags.HasError() || !strings.Contains(diags[0].Summary, "default_msg = false") {
		t.Fatalf("the same conflict must fail before update, got %v", diags)
	}

	// An eventsource that resolved to a non-trigger source at apply time.
	rawES := map[string]interface{}{"name": "a", "eventsource": 1,
		"operation": []interface{}{map[string]interface{}{"users": []interface{}{"1"}}}}
	if diags := r.CreateContext(context.Background(), schema.TestResourceDataRaw(t, r.Schema, rawES), c); !diags.HasError() || !strings.Contains(diags[0].Summary, "eventsource 1 is not supported") {
		t.Fatalf("a resolved non-trigger eventsource must fail before create, got %v", diags)
	}

	// An operationtype that resolved to something other than "send message":
	// creating it would immediately strand the action as unmanageable.
	rawOT := map[string]interface{}{"name": "a",
		"operation": []interface{}{map[string]interface{}{"operationtype": 1, "users": []interface{}{"1"}}}}
	if diags := r.CreateContext(context.Background(), schema.TestResourceDataRaw(t, r.Schema, rawOT), c); !diags.HasError() || !strings.Contains(diags[0].Summary, "operationtype 1 is not supported") {
		t.Fatalf("a resolved unsupported operationtype must fail before create, got %v", diags)
	}

	// The remaining enums, resolved to unsupported variants at apply time.
	rawET := map[string]interface{}{"name": "a", "evaltype": 3,
		"operation": []interface{}{map[string]interface{}{"users": []interface{}{"1"}}}}
	if diags := r.CreateContext(context.Background(), schema.TestResourceDataRaw(t, r.Schema, rawET), c); !diags.HasError() || !strings.Contains(diags[0].Summary, "evaltype 3 is not supported") {
		t.Fatalf("a resolved custom evaltype must fail before create, got %v", diags)
	}
	rawCT := map[string]interface{}{"name": "a",
		"operation": []interface{}{map[string]interface{}{"users": []interface{}{"1"}}},
		"condition": []interface{}{map[string]interface{}{"conditiontype": 5, "value": "x"}}}
	if diags := r.CreateContext(context.Background(), schema.TestResourceDataRaw(t, r.Schema, rawCT), c); !diags.HasError() || !strings.Contains(diags[0].Summary, "conditiontype 5 is not supported") {
		t.Fatalf("a resolved unsupported conditiontype must fail before create, got %v", diags)
	}

	// Durations and step bounds that resolved to invalid values at apply time.
	rawEsc := map[string]interface{}{"name": "a", "esc_period": "10",
		"operation": []interface{}{map[string]interface{}{"users": []interface{}{"1"}}}}
	if diags := r.CreateContext(context.Background(), schema.TestResourceDataRaw(t, r.Schema, rawEsc), c); !diags.HasError() || !strings.Contains(diags[0].Summary, "between 60 seconds") {
		t.Fatalf("a resolved too-short esc_period must fail before create, got %v", diags)
	}
	rawOpEsc := map[string]interface{}{"name": "a",
		"operation": []interface{}{map[string]interface{}{"users": []interface{}{"1"}, "esc_period": "30"}}}
	if diags := r.CreateContext(context.Background(), schema.TestResourceDataRaw(t, r.Schema, rawOpEsc), c); !diags.HasError() || !strings.Contains(diags[0].Summary, "between 60 seconds") {
		t.Fatalf("a resolved invalid operation esc_period must fail before create, got %v", diags)
	}
	rawFrom := map[string]interface{}{"name": "a",
		"operation": []interface{}{map[string]interface{}{"users": []interface{}{"1"}, "esc_step_from": 0}}}
	if diags := r.CreateContext(context.Background(), schema.TestResourceDataRaw(t, r.Schema, rawFrom), c); !diags.HasError() || !strings.Contains(diags[0].Summary, "esc_step_from must be >= 1") {
		t.Fatalf("a resolved zero esc_step_from must fail before create, got %v", diags)
	}
}

func TestActionUpdate_RefusesExternalUnmanagedShapes(t *testing.T) {
	// Shapes added outside Terraform since the last refresh must not be
	// silently dropped by the wholesale action.update.
	base := strings.NewReplacer("%OP%", "0", "%DM%", "1").Replace(actionFixture)
	fixture := strings.Replace(base, `"operations":[`, `"recovery_operations":[{"operationid":"99"}],"operations":[`, 1)
	c := fixtureServer(t, "action.get", fixture)
	d := schema.TestResourceDataRaw(t, resourceAction().Schema, map[string]interface{}{
		"name": "a", "operation": []interface{}{map[string]interface{}{"users": []interface{}{"1"}}}})
	d.SetId("10")
	diags := resourceAction().UpdateContext(context.Background(), d, c)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "recovery or update operations") || !strings.Contains(diags[0].Summary, "terraform state rm") {
		t.Fatalf("external recovery operations must refuse the update, got %v", diags)
	}

	// The preflight refuses exactly what Read refuses - e.g. a custom filter
	// expression that appeared externally between plan and apply.
	evalFixture := strings.Replace(base, `"evaltype":"2"`, `"evaltype":"3"`, 1)
	c2 := fixtureServer(t, "action.get", evalFixture)
	d2 := schema.TestResourceDataRaw(t, resourceAction().Schema, map[string]interface{}{
		"name": "a", "operation": []interface{}{map[string]interface{}{"users": []interface{}{"1"}}}})
	d2.SetId("10")
	if diags := resourceAction().UpdateContext(context.Background(), d2, c2); !diags.HasError() || !strings.Contains(diags[0].Summary, "custom condition expression") {
		t.Fatalf("external evaltype 3 must refuse the update, got %v", diags)
	}
}

func TestMediaTypeApplyValidation_RejectsResolvedConflicts(t *testing.T) {
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		t.Errorf("no API call expected, got %s", req.Method)
		return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	r := resourceMediaType()
	// smtp_authentication resolved to 0 while credentials are configured: the
	// password must not be silently dropped from the SMTP configuration.
	raw := map[string]interface{}{"name": "m", "type": 0, "smtp_server": "s", "smtp_helo": "h", "smtp_email": "e",
		"smtp_authentication": 0, "username": "u", "password": "p"}
	if diags := r.CreateContext(context.Background(), schema.TestResourceDataRaw(t, r.Schema, raw), c); !diags.HasError() || !strings.Contains(diags[0].Summary, "smtp_authentication = 1") {
		t.Fatalf("credentials with resolved smtp_authentication=0 must fail before create, got %v", diags)
	}
	d := schema.TestResourceDataRaw(t, r.Schema, raw)
	d.SetId("45")
	if diags := r.UpdateContext(context.Background(), d, c); !diags.HasError() || !strings.Contains(diags[0].Summary, "smtp_authentication = 1") {
		t.Fatalf("the same conflict must fail before update, got %v", diags)
	}

	// Event menu fields next to a show_event_menu that resolved to false.
	raw2 := map[string]interface{}{"name": "m", "type": 4, "script": "x", "event_menu_url": "https://x"}
	if diags := r.CreateContext(context.Background(), schema.TestResourceDataRaw(t, r.Schema, raw2), c); !diags.HasError() || !strings.Contains(diags[0].Summary, "requires show_event_menu") {
		t.Fatalf("event_menu_url with show_event_menu resolved to false must fail, got %v", diags)
	}

	// A type that resolved to an unsupported transport at apply time.
	raw3 := map[string]interface{}{"name": "m", "type": 3, "script": "x"}
	if diags := r.CreateContext(context.Background(), schema.TestResourceDataRaw(t, r.Schema, raw3), c); !diags.HasError() || !strings.Contains(diags[0].Summary, "type 3 is not supported") {
		t.Fatalf("a resolved unsupported type must fail before create, got %v", diags)
	}
	d3 := schema.TestResourceDataRaw(t, r.Schema, raw3)
	d3.SetId("9")
	if diags := r.UpdateContext(context.Background(), d3, c); !diags.HasError() || !strings.Contains(diags[0].Summary, "type 3 is not supported") {
		t.Fatalf("the same resolved type must fail before update, got %v", diags)
	}
}

func TestHostGroupCreate_AmbiguousOutcomeHint(t *testing.T) {
	// A transport failure leaves the create outcome unknown: the diagnostics
	// must warn that the object may exist and suggest an import.
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) { return nil, nil })
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	s.Close()
	d := schema.TestResourceDataRaw(t, resourceHostGroup().Schema, map[string]interface{}{"name": "g"})
	diags := resourceHostGroup().CreateContext(context.Background(), d, c)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "outcome is unknown") {
		t.Fatalf("a transport failure during create must carry the import hint, got %v", diags)
	}
}

func TestExpandHost_NameFollowsResolvedHost(t *testing.T) {
	// CustomizeDiff cannot normalise the visible name while `host` is unknown
	// in the plan; expandHost must fall back to the RESOLVED host instead of
	// the stale name carried in state.
	d := schema.TestResourceDataRaw(t, resourceHost().Schema, map[string]interface{}{"host": "resolved-host", "groups": []interface{}{"2"}})
	if err := d.Set("name", "stale-visible-name"); err != nil {
		t.Fatal(err)
	}
	if spec := expandHost(d); spec.Name != "resolved-host" {
		t.Fatalf("an unconfigured visible name must follow the resolved host, got %q", spec.Name)
	}
}

func TestPartialStateOnFailedUpdates(t *testing.T) {
	// The production path: Resource.Diff over (previous state, planned config)
	// followed by Resource.Apply. After a failed mutation the returned state
	// must still carry the complete PREVIOUS values - not merely lack the
	// planned ones, and never be empty (that would plan a recreate).
	boom := &JsonRpcError{Code: -32500, Message: "Application error.", Data: "boom"}
	actionBase := strings.NewReplacer("%OP%", "0", "%DM%", "1").Replace(actionFixture)
	emailFixture := `[{"mediatypeid":"45","type":"0","name":"old-name","status":"0",
		"smtp_server":"old.mail","smtp_port":"587","smtp_helo":"h","smtp_email":"e@x","smtp_security":"0","smtp_verify_peer":"0","smtp_verify_host":"0",
		"smtp_authentication":"1","username":"u","passwd":"old-secret","exec_path":"","gsm_modem":"","script":"","timeout":"30s","content_type":"1","parameters":[]}]`

	cases := []struct {
		name     string
		resource *schema.Resource
		old      map[string]string
		planned  map[string]interface{}
		handler  func(req rpcRequest) (interface{}, *JsonRpcError)
	}{
		{"host group", resourceHostGroup(),
			map[string]string{"name": "old-name"},
			map[string]interface{}{"name": "planned"},
			func(req rpcRequest) (interface{}, *JsonRpcError) { return nil, boom }},
		{"action", resourceAction(),
			map[string]string{
				"name": "old-name", "eventsource": "0", "enabled": "true", "esc_period": "1h", "evaltype": "0",
				"pause_suppressed": "true", "pause_symptoms": "true", "notify_if_canceled": "true",
				"condition.#": "0",
				"operation.#": "1", "operation.0.operationtype": "0", "operation.0.esc_period": "0",
				"operation.0.esc_step_from": "1", "operation.0.esc_step_to": "1",
				"operation.0.mediatypeid": "0", "operation.0.default_msg": "true",
				"operation.0.subject": "", "operation.0.message": "",
				"operation.0.user_groups.#": "0",
				// 915405929 is the SDK set hash of the string element "1".
				"operation.0.users.#": "1", "operation.0.users.915405929": "1",
			},
			map[string]interface{}{"name": "planned",
				"operation": []interface{}{map[string]interface{}{"users": []interface{}{"1"}}}},
			func(req rpcRequest) (interface{}, *JsonRpcError) {
				if req.Method == "action.get" {
					return json.RawMessage(actionBase), nil
				}
				return nil, boom
			}},
		{"media type", resourceMediaType(),
			map[string]string{
				"name": "old-name", "type": "0", "enabled": "true",
				"smtp_server": "old.mail", "smtp_port": "587", "smtp_helo": "h", "smtp_email": "e@x",
				"smtp_security": "0", "smtp_verify_peer": "false", "smtp_verify_host": "false",
				"smtp_authentication": "1", "username": "u", "password": "old-secret",
				"exec_path": "", "gsm_modem": "", "script": "", "timeout": "30s",
				"description": "", "max_sessions": "1", "max_attempts": "3", "attempt_interval": "10s",
				"content_type": "1", "process_tags": "false", "show_event_menu": "false",
				"event_menu_url": "", "event_menu_name": "", "parameter.#": "0",
			},
			map[string]interface{}{"name": "planned", "type": 0,
				"smtp_server": "old.mail", "smtp_port": 587, "smtp_helo": "h", "smtp_email": "e@x",
				"smtp_authentication": 1, "username": "u", "password": "old-secret", "content_type": 1},
			func(req rpcRequest) (interface{}, *JsonRpcError) {
				if req.Method == "mediatype.get" {
					return json.RawMessage(emailFixture), nil
				}
				return nil, boom
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRPCServer(t, tc.handler)
			c := newTestClient(t, s, ClientConfig{APIToken: "t"})
			state := &terraform.InstanceState{ID: "45", Attributes: tc.old}
			diff, err := tc.resource.Diff(context.Background(), state, terraform.NewResourceConfigRaw(tc.planned), nil)
			if err != nil {
				t.Fatal(err)
			}
			if diff == nil {
				t.Fatal("expected a non-empty diff")
			}
			newState, diags := tc.resource.Apply(context.Background(), state, diff, c)
			if !diags.HasError() {
				t.Fatalf("the failing update must surface an error, got %v", diags)
			}
			if newState == nil {
				t.Fatal("the state must survive a failed update, got nil")
			}
			for k, v := range tc.old {
				if got := newState.Attributes[k]; got != v {
					t.Fatalf("attribute %s must keep its previous value %q, got %q (attrs: %v)", k, v, got, newState.Attributes)
				}
			}
		})
	}
}

func TestHostUpdate_InterfaceDeleteFailureKeepsID(t *testing.T) {
	// Removing the address from the configuration takes the interface-delete
	// branch; its failure must surface with the ID kept, like the other
	// partial-failure paths.
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "host.get":
			return json.RawMessage(`[{"hostid":"1","host":"h","name":"h","status":"0","flags":"0","description":"",
				"parentTemplates":[],"hostgroups":[{"groupid":"2"}],
				"interfaces":[{"interfaceid":"5","type":"1","main":"1","useip":"1","ip":"192.0.2.1","dns":"","port":"10050"}]}]`), nil
		case "host.update":
			return map[string][]string{"hostids": {"1"}}, nil
		case "hostinterface.delete":
			return nil, &JsonRpcError{Code: -32500, Message: "Application error.", Data: "boom"}
		}
		t.Errorf("unexpected method %s", req.Method)
		return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	d := schema.TestResourceDataRaw(t, resourceHost().Schema, map[string]interface{}{"host": "h", "groups": []interface{}{"2"}})
	d.SetId("1")
	diags := resourceHost().UpdateContext(context.Background(), d, c)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "removing agent interface") || d.Id() != "1" {
		t.Fatalf("a failed interface delete must surface and keep the ID, got %v id=%q", diags, d.Id())
	}
	if len(s.calls("hostinterface.delete")) != 1 {
		t.Fatal("the interface delete must have been attempted exactly once")
	}
}

func TestHostUpdate_InterfaceVanishedMidUpdate(t *testing.T) {
	// The agent interface was deleted externally between the preflight read
	// and hostinterface.update: the operator gets the friendly "vanished"
	// error, not a raw permissions-or-missing message.
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "host.get":
			return json.RawMessage(`[{"hostid":"1","host":"h","name":"h","status":"0","flags":"0","description":"",
				"parentTemplates":[],"hostgroups":[{"groupid":"2"}],
				"interfaces":[{"interfaceid":"5","type":"1","main":"1","useip":"1","ip":"192.0.2.1","dns":"","port":"10050"}]}]`), nil
		case "host.update":
			return map[string][]string{"hostids": {"1"}}, nil
		case "hostinterface.update":
			return nil, &JsonRpcError{Code: -32500, Message: "Application error.", Data: objectMissing}
		}
		t.Errorf("unexpected method %s", req.Method)
		return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	d := schema.TestResourceDataRaw(t, resourceHost().Schema, map[string]interface{}{"host": "h", "groups": []interface{}{"2"}, "ip": "192.0.2.9"})
	d.SetId("1")
	diags := resourceHost().UpdateContext(context.Background(), d, c)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "vanished") || d.Id() != "1" {
		t.Fatalf("a vanished interface must produce the friendly error with the ID kept, got %v id=%q", diags, d.Id())
	}
}

func TestHostUpdate_FailedFinalReadSurfacesError(t *testing.T) {
	// The mutations succeeded but the confirming Read fails on transport: the
	// error must surface with the ID kept. Partial mode is already off at
	// that point, so the planned values persist until the next refresh
	// reconciles them - inherent to SDKv2 and acceptable, because the
	// mutation itself DID succeed.
	var gets atomic.Int32
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "host.get":
			if gets.Add(1) == 1 { // update preflight
				return json.RawMessage(`[{"hostid":"1","host":"h","name":"h","status":"0","flags":"0","description":"",
					"parentTemplates":[],"hostgroups":[{"groupid":"2"}],
					"interfaces":[{"interfaceid":"5","type":"1","main":"1","useip":"1","ip":"192.0.2.1","dns":"","port":"10050"}]}]`), nil
			}
			return nil, &JsonRpcError{Code: -32500, Message: "Application error.", Data: "db down"}
		case "host.update":
			return map[string][]string{"hostids": {"1"}}, nil
		case "hostinterface.update":
			return map[string][]string{"interfaceids": {"5"}}, nil
		}
		t.Errorf("unexpected method %s", req.Method)
		return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	d := schema.TestResourceDataRaw(t, resourceHost().Schema, map[string]interface{}{"host": "h", "groups": []interface{}{"2"}, "ip": "192.0.2.9"})
	d.SetId("1")
	diags := resourceHost().UpdateContext(context.Background(), d, c)
	if !diags.HasError() || d.Id() != "1" {
		t.Fatalf("a failing final read must surface an error and keep the ID, got %v id=%q", diags, d.Id())
	}
}

func TestHostUpdate_VanishedHost(t *testing.T) {
	// The host was deleted externally between plan and apply: the error must
	// say so instead of a bare "object not found", and the ID must survive so
	// the next refresh clears the state and recreates the host.
	c := fixtureServer(t, "host.get", `[]`)
	d := schema.TestResourceDataRaw(t, resourceHost().Schema, map[string]interface{}{"host": "h", "groups": []interface{}{"2"}})
	d.SetId("1")
	diags := resourceHost().UpdateContext(context.Background(), d, c)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "deleted externally") || d.Id() != "1" {
		t.Fatalf("a vanished host must produce a clear error and keep the ID, got %v id=%q", diags, d.Id())
	}
}

func TestHostResource_DeleteIdempotent(t *testing.T) {
	// First delete succeeds; a second delete finds the host gone on the
	// preflight read and returns success without a mutating call.
	var gone bool
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "host.delete":
			gone = true
			return map[string][]string{"hostids": {"7"}}, nil
		case "host.get":
			if gone {
				return json.RawMessage(`[]`), nil
			}
			return json.RawMessage(`[{"hostid":"7","host":"h","name":"h","status":"0","flags":"0","description":"",
				"parentTemplates":[],"hostgroups":[{"groupid":"2"}],"interfaces":[]}]`), nil
		}
		t.Errorf("unexpected method %s", req.Method)
		return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	r := resourceHost()
	d := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{"host": "h", "groups": []interface{}{"2"}})
	d.SetId("7")
	if diags := r.DeleteContext(context.Background(), d, c); diags.HasError() {
		t.Fatalf("first delete: %v", diags)
	}
	d.SetId("7")
	if diags := r.DeleteContext(context.Background(), d, c); diags.HasError() {
		t.Fatalf("second delete must be idempotent (object already gone), got %v", diags)
	}
	if calls := s.calls("host.delete"); len(calls) != 1 {
		t.Fatalf("want exactly 1 host.delete (the second run must stop at the preflight), got %d", len(calls))
	}
}

func TestHostMutations_RefuseDiscoveredHost(t *testing.T) {
	// The LLD barrier must hold on Update and Delete too: with -refresh=false
	// Read never runs, so a state inherited from v0.1 could otherwise mutate
	// or destroy a host owned by a discovery rule.
	lld := `[{"hostid":"7","host":"lld","name":"lld","status":"0","flags":"4","description":"",
		"parentTemplates":[],"hostgroups":[{"groupid":"2"}],"interfaces":[]}]`
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		if req.Method != "host.get" {
			t.Errorf("no mutating RPC may run against an LLD host, got %s", req.Method)
			return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
		}
		return json.RawMessage(lld), nil
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	r := resourceHost()

	d := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{"host": "lld", "groups": []interface{}{"2"}})
	d.SetId("7")
	if diags := r.UpdateContext(context.Background(), d, c); !diags.HasError() || !strings.Contains(diags[0].Summary, "low-level discovery") {
		t.Fatalf("updating an LLD host must be refused, got %v", diags)
	}

	d2 := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{"host": "lld", "groups": []interface{}{"2"}})
	d2.SetId("7")
	if diags := r.DeleteContext(context.Background(), d2, c); !diags.HasError() || !strings.Contains(diags[0].Summary, "low-level discovery") {
		t.Fatalf("deleting an LLD host must be refused, got %v", diags)
	}

	// A response WITHOUT any flags field cannot prove the host is not
	// LLD-owned: mutations are fail-closed on it (Read stays tolerant).
	noFlags := `[{"hostid":"7","host":"h","name":"h","status":"0","description":"",
		"parentTemplates":[],"hostgroups":[{"groupid":"2"}],"interfaces":[]}]`
	c2 := fixtureServer(t, "host.get", noFlags)
	d3 := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{"host": "h", "groups": []interface{}{"2"}})
	d3.SetId("7")
	if diags := r.UpdateContext(context.Background(), d3, c2); !diags.HasError() || !strings.Contains(diags[0].Summary, "no flags field") {
		t.Fatalf("a flagless response must refuse the update, got %v", diags)
	}
	d4 := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{"host": "h", "groups": []interface{}{"2"}})
	d4.SetId("7")
	if diags := r.DeleteContext(context.Background(), d4, c2); !diags.HasError() || !strings.Contains(diags[0].Summary, "no flags field") {
		t.Fatalf("a flagless response must refuse the delete, got %v", diags)
	}
}

func TestHostRead_RefusesDiscoveredHost(t *testing.T) {
	c := fixtureServer(t, "host.get", `[{"hostid":"1","host":"lld","name":"lld","status":"0","flags":"4","description":"",
		"parentTemplates":[],"hostgroups":[{"groupid":"2"}],"interfaces":[]}]`)
	d := schema.TestResourceDataRaw(t, resourceHost().Schema, map[string]interface{}{})
	d.SetId("1")
	diags := resourceHost().ReadContext(context.Background(), d, c)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "low-level discovery") || d.Id() != "1" {
		t.Fatalf("a discovered host must be refused and keep the ID, got %v", diags)
	}
}

func TestHostGroupResource_CRUD(t *testing.T) {
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "hostgroup.create", "hostgroup.update", "hostgroup.delete":
			return map[string][]string{"groupids": {"42"}}, nil
		case "hostgroup.get":
			return json.RawMessage(`[{"groupid":"42","name":"g2"}]`), nil
		}
		return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	r := resourceHostGroup()

	d := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{"name": "g"})
	if diags := r.CreateContext(context.Background(), d, c); diags.HasError() {
		t.Fatal(diags)
	}
	if d.Id() != "42" || d.Get("name") != "g2" { // Read after create reflects the API
		t.Fatalf("create: id=%q name=%q", d.Id(), d.Get("name"))
	}
	if diags := r.UpdateContext(context.Background(), d, c); diags.HasError() {
		t.Fatal(diags)
	}
	if calls := s.calls("hostgroup.update"); len(calls) != 1 {
		t.Fatalf("want exactly one hostgroup.update, got %d", len(calls))
	} else {
		var params map[string]string
		_ = json.Unmarshal(calls[0].Params, &params)
		// After create the state reflects the API ("g2"), so the update carries it.
		if params["name"] != "g2" || params["groupid"] != "42" {
			t.Fatalf("update payload must carry the configured name, got %v", params)
		}
	}
	if diags := r.DeleteContext(context.Background(), d, c); diags.HasError() {
		t.Fatal(diags)
	}
	if len(s.calls("hostgroup.delete")) != 1 {
		t.Fatal("delete must call hostgroup.delete")
	}
}

func TestActionResource_UpdateSendsClearedRecipients(t *testing.T) {
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "action.update":
			return map[string][]string{"actionids": {"10"}}, nil
		case "action.get":
			return json.RawMessage(strings.NewReplacer("%OP%", "0", "%DM%", "1").Replace(actionFixture)), nil
		}
		return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	r := resourceAction()
	d := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{
		"name":      "exp-a",
		"operation": []interface{}{map[string]interface{}{"users": []interface{}{"1"}}},
	})
	d.SetId("10")
	if diags := r.UpdateContext(context.Background(), d, c); diags.HasError() {
		t.Fatal(diags)
	}
	var params map[string]interface{}
	_ = json.Unmarshal(s.calls("action.update")[0].Params, &params)
	op := params["operations"].([]interface{})[0].(map[string]interface{})
	if grp, ok := op["opmessage_grp"].([]interface{}); !ok || len(grp) != 0 {
		t.Fatalf("removed groups must be sent as an empty array, got %#v", op["opmessage_grp"])
	}
	if usr := op["opmessage_usr"].([]interface{}); len(usr) != 1 {
		t.Fatalf("users must be sent, got %#v", op["opmessage_usr"])
	}
}

func TestMediaTypeResource_DeleteIdempotent(t *testing.T) {
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "mediatype.delete":
			return nil, &JsonRpcError{Code: -32500, Message: "Application error.", Data: objectMissing}
		case "mediatype.get":
			return json.RawMessage(`[]`), nil
		}
		return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	d := schema.TestResourceDataRaw(t, resourceMediaType().Schema, map[string]interface{}{"name": "m", "type": 4, "script": "x"})
	d.SetId("9")
	if diags := resourceMediaType().DeleteContext(context.Background(), d, c); diags.HasError() {
		t.Fatalf("deleting an already removed media type must succeed, got %v", diags)
	}
}

func TestMediaTypeUpdate_RefusesExternallyGainedScriptParameters(t *testing.T) {
	// Parameters appeared on a script media type between plan and apply: the
	// update must be refused BEFORE mutating (the final Read would refuse the
	// object only after the rename already happened).
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		if req.Method != "mediatype.get" {
			t.Errorf("no mutation expected, got %s", req.Method)
			return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
		}
		return json.RawMessage(`[{"mediatypeid":"9","type":"1","name":"s","status":"0",
			"smtp_server":"","smtp_port":"25","smtp_helo":"","smtp_email":"","smtp_security":"0","smtp_verify_peer":"0","smtp_verify_host":"0",
			"smtp_authentication":"0","username":"","passwd":"","exec_path":"x.sh","gsm_modem":"","script":"","timeout":"30s",
			"parameters":[{"sortorder":"0","value":"{ALERT.SENDTO}"}]}]`), nil
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	r := resourceMediaType()
	d := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{"name": "s-renamed", "type": 1, "exec_path": "x.sh"})
	d.SetId("9")
	diags := r.UpdateContext(context.Background(), d, c)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "script media type with parameters") || !strings.Contains(diags[0].Summary, "terraform state rm") {
		t.Fatalf("externally gained script parameters must refuse the update, got %v", diags)
	}
}

func TestMediaTypeUpdate_ClearsParametersOnExternalTypeDrift(t *testing.T) {
	// The API-current object is a webhook with parameters (drift); applying a
	// script configuration must clear them based on the CURRENT type, or the
	// next Read would refuse the leftover script-with-parameters shape.
	var updated map[string]interface{}
	gets := 0
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "mediatype.get":
			gets++
			if gets == 1 {
				return json.RawMessage(`[{"mediatypeid":"9","type":"4","name":"wh","status":"0",
					"smtp_server":"","smtp_port":"25","smtp_helo":"","smtp_email":"","smtp_security":"0","smtp_verify_peer":"0","smtp_verify_host":"0",
					"smtp_authentication":"0","username":"","passwd":"","exec_path":"","gsm_modem":"","script":"return 1;","timeout":"30s",
					"parameters":[{"name":"a","value":"b"}]}]`), nil
			}
			return json.RawMessage(`[{"mediatypeid":"9","type":"1","name":"wh","status":"0",
				"smtp_server":"","smtp_port":"25","smtp_helo":"","smtp_email":"","smtp_security":"0","smtp_verify_peer":"0","smtp_verify_host":"0",
				"smtp_authentication":"0","username":"","passwd":"","exec_path":"x.sh","gsm_modem":"","script":"","timeout":"30s",
				"parameters":[]}]`), nil
		case "mediatype.update":
			_ = json.Unmarshal(req.Params, &updated)
			return map[string][]string{"mediatypeids": {"9"}}, nil
		}
		t.Errorf("unexpected method %s", req.Method)
		return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	r := resourceMediaType()
	d := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{"name": "wh", "type": 1, "exec_path": "x.sh"})
	d.SetId("9")
	if diags := r.UpdateContext(context.Background(), d, c); diags.HasError() {
		t.Fatal(diags)
	}
	params, ok := updated["parameters"].([]interface{})
	if !ok || len(params) != 0 {
		t.Fatalf("a type change detected against the API state must clear parameters, got %#v", updated["parameters"])
	}
}

func TestMediaTypeResource_EmailUpdatePayload(t *testing.T) {
	var updated map[string]interface{}
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "mediatype.update":
			_ = json.Unmarshal(req.Params, &updated)
			return map[string][]string{"mediatypeids": {"45"}}, nil
		case "mediatype.get":
			return json.RawMessage(`[{"mediatypeid":"45","type":"0","name":"mail","status":"0",
				"smtp_server":"mail.x","smtp_port":"2525","smtp_helo":"x","smtp_email":"a@x","smtp_security":"1",
				"smtp_verify_peer":"0","smtp_verify_host":"0","smtp_authentication":"1","username":"u","passwd":"pw2",
				"exec_path":"","gsm_modem":"","script":"","timeout":"30s","content_type":"0","parameters":[]}]`), nil
		}
		t.Errorf("unexpected method %s", req.Method)
		return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	r := resourceMediaType()
	d := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{
		"name": "mail", "type": 0, "smtp_server": "mail.x", "smtp_helo": "x", "smtp_email": "a@x",
		"smtp_port": 2525, "smtp_security": 1, "smtp_authentication": 1,
		"username": "u", "password": "pw2", "content_type": 0,
	})
	d.SetId("45")
	if diags := r.UpdateContext(context.Background(), d, c); diags.HasError() {
		t.Fatal(diags)
	}
	want := map[string]interface{}{"mediatypeid": "45", "smtp_port": "2525", "passwd": "pw2", "content_type": "0", "script": "",
		"smtp_server": "mail.x", "smtp_helo": "x", "smtp_email": "a@x",
		"smtp_security": "1", "smtp_verify_peer": "0", "smtp_verify_host": "0",
		"smtp_authentication": "1", "username": "u"}
	for k, v := range want {
		if updated[k] != v {
			t.Errorf("update payload %s: want %v, got %v", k, v, updated[k])
		}
	}
	if d.Get("password") != "pw2" || d.Get("smtp_port") != 2525 {
		t.Errorf("round-trip after update: %v %v", d.Get("password"), d.Get("smtp_port"))
	}
}

func TestMediaTypeRead_RefusesScriptWithParameters(t *testing.T) {
	c := fixtureServer(t, "mediatype.get", `[{"mediatypeid":"9","type":"1","name":"s","status":"0",
		"smtp_server":"","smtp_port":"25","smtp_helo":"","smtp_email":"","smtp_security":"0","smtp_verify_peer":"0","smtp_verify_host":"0",
		"smtp_authentication":"0","username":"","passwd":"","exec_path":"x.sh","gsm_modem":"","script":"","timeout":"30s",
		"parameters":[{"sortorder":"0","value":"{ALERT.SENDTO}"}]}]`)
	d := schema.TestResourceDataRaw(t, resourceMediaType().Schema, map[string]interface{}{})
	d.SetId("9")
	diags := resourceMediaType().ReadContext(context.Background(), d, c)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "script media type with parameters") || d.Id() != "9" {
		t.Fatalf("script parameters must be refused and the ID kept, got %v id=%q", diags, d.Id())
	}
}

func TestHostUpdate_TemplatesClearFromAPIState(t *testing.T) {
	// A template linked outside Terraform must be cleared (not only unlinked).
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "host.update":
			return map[string][]string{"hostids": {"1"}}, nil
		case "hostinterface.update":
			return map[string][]string{"interfaceids": {"5"}}, nil
		case "host.get":
			return json.RawMessage(`[{"hostid":"1","host":"h","name":"h","status":"0","flags":"0","description":"",
				"parentTemplates":[{"templateid":"10001"},{"templateid":"10050"}],"hostgroups":[{"groupid":"2"}],
				"interfaces":[{"interfaceid":"5","type":"1","main":"1","useip":"1","ip":"192.0.2.1","dns":"","port":"10050"}]}]`), nil
		}
		return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	r := resourceHost()
	d := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{"host": "h", "groups": []interface{}{"2"}, "templates": []interface{}{"10001"}, "ip": "192.0.2.1"})
	d.SetId("1")
	if diags := r.UpdateContext(context.Background(), d, c); diags.HasError() {
		t.Fatal(diags)
	}
	var params map[string]interface{}
	_ = json.Unmarshal(s.calls("host.update")[0].Params, &params)
	clear, _ := params["templates_clear"].([]interface{})
	if len(clear) != 1 || clear[0].(map[string]interface{})["templateid"] != "10050" {
		t.Fatalf("want templates_clear [10050] computed from the API state, got %v", params["templates_clear"])
	}
}

func TestHostUpdate_PartialFailureKeepsID(t *testing.T) {
	// host.update succeeds, the interface update fails: the error must surface
	// and the ID stay, so the next apply retries the interface step.
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "host.update":
			return map[string][]string{"hostids": {"1"}}, nil
		case "hostinterface.update":
			return nil, &JsonRpcError{Code: -32500, Message: "Application error.", Data: "boom"}
		case "host.get":
			return json.RawMessage(`[{"hostid":"1","host":"h","name":"h","status":"0","flags":"0","description":"",
				"parentTemplates":[],"hostgroups":[{"groupid":"2"}],
				"interfaces":[{"interfaceid":"5","type":"1","main":"1","useip":"1","ip":"192.0.2.1","dns":"","port":"10050"}]}]`), nil
		}
		return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	d := schema.TestResourceDataRaw(t, resourceHost().Schema, map[string]interface{}{"host": "h", "groups": []interface{}{"2"}, "ip": "192.0.2.9"})
	d.SetId("1")
	diags := resourceHost().UpdateContext(context.Background(), d, c)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "agent interface") || d.Id() != "1" {
		t.Fatalf("partial failure must surface and keep the ID, got %v id=%q", diags, d.Id())
	}
	if len(s.calls("hostinterface.update")) != 1 {
		t.Fatal("the interface update must have been attempted once")
	}
	// SDKv2 would otherwise persist the planned values despite the failure;
	// partial mode must keep the previous state (the planned IP is not saved).
	if st := d.State(); st == nil || st.Attributes["ip"] == "192.0.2.9" {
		t.Fatalf("planned values must not be written to state after a failed update, got %+v", st)
	}
}

func TestHostGroupUpdate_VanishedGroup(t *testing.T) {
	// The group was deleted externally between plan and apply: the error must
	// say so instead of surfacing the raw permissions-or-missing API message.
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		return nil, &JsonRpcError{Code: -32500, Message: "Application error.", Data: objectMissing}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	d := schema.TestResourceDataRaw(t, resourceHostGroup().Schema, map[string]interface{}{"name": "renamed"})
	d.SetId("42")
	diags := resourceHostGroup().UpdateContext(context.Background(), d, c)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "deleted externally") || d.Id() != "42" {
		t.Fatalf("a vanished group must produce a clear error with the ID kept, got %v id=%q", diags, d.Id())
	}
}

func TestActionResource_DeleteIdempotent(t *testing.T) {
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "action.delete":
			return nil, &JsonRpcError{Code: -32500, Message: "Application error.", Data: objectMissing}
		case "action.get":
			return json.RawMessage(`[]`), nil
		}
		return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	d := schema.TestResourceDataRaw(t, resourceAction().Schema, map[string]interface{}{"name": "a"})
	d.SetId("9")
	if diags := resourceAction().DeleteContext(context.Background(), d, c); diags.HasError() {
		t.Fatalf("deleting an already removed action must succeed, got %v", diags)
	}
}

func TestFlattenAction_NonNumericRefusesWithHint(t *testing.T) {
	_, err := flattenAction(&Action{EventSource: "zero"})
	if err == nil || !strings.Contains(err.Error(), "terraform state rm") {
		t.Fatalf("a non-numeric eventsource must refuse with the state-rm hint, got %v", err)
	}
}

func TestHostGroupDelete_NonEmptyGroupHint(t *testing.T) {
	// Zabbix refuses to delete a group that still contains hosts; the
	// operator should see why, not a bare application error.
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		if req.Method != "hostgroup.delete" {
			t.Errorf("unexpected method %s", req.Method)
		}
		return nil, &JsonRpcError{Code: -32500, Message: "Application error.", Data: `host "h1" cannot be without host group`}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	d := schema.TestResourceDataRaw(t, resourceHostGroup().Schema, map[string]interface{}{"name": "g"})
	d.SetId("4")
	diags := resourceHostGroup().DeleteContext(context.Background(), d, c)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "still contains hosts") || d.Id() != "4" {
		t.Fatalf("a refused group delete must carry the non-empty-group hint and keep the ID, got %v id=%q", diags, d.Id())
	}
}

func TestHostGroupCreate_EmptyReadKeepsID(t *testing.T) {
	// A read that comes back empty right after a successful create is a
	// consistency problem; forgetting the ID would orphan the new object.
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "hostgroup.create":
			return map[string][]string{"groupids": {"42"}}, nil
		case "hostgroup.get":
			return json.RawMessage(`[]`), nil
		}
		t.Errorf("unexpected method %s", req.Method)
		return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	d := schema.TestResourceDataRaw(t, resourceHostGroup().Schema, map[string]interface{}{"name": "g"})
	diags := resourceHostGroup().CreateContext(context.Background(), d, c)
	if !diags.HasError() || d.Id() != "42" {
		t.Fatalf("an empty read after create must error and keep the ID, got diags=%v id=%q", diags, d.Id())
	}
}

func TestHostUpdate_NoAgentInterface(t *testing.T) {
	// SNMP-only host: the agent interface is created from the configuration;
	// the SNMP interface must never be touched.
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "host.update":
			return map[string][]string{"hostids": {"1"}}, nil
		case "hostinterface.create":
			return map[string][]string{"interfaceids": {"6"}}, nil
		case "host.get":
			return json.RawMessage(`[{"hostid":"1","host":"snmp-only","name":"snmp-only","status":"0","flags":"0","description":"",
				"parentTemplates":[],"hostgroups":[{"groupid":"2"}],
				"interfaces":[{"interfaceid":"5","type":"2","main":"1","useip":"1","ip":"10.0.0.2","dns":"","port":"161"}]}]`), nil
		}
		t.Errorf("unexpected method %s", req.Method)
		return nil, &JsonRpcError{Code: -32601, Message: "Method not found."}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	r := resourceHost()
	d := schema.TestResourceDataRaw(t, r.Schema, map[string]interface{}{"host": "snmp-only", "groups": []interface{}{"2"}, "ip": "192.0.2.1"})
	d.SetId("1")
	if diags := r.UpdateContext(context.Background(), d, c); diags.HasError() {
		t.Fatalf("a missing agent interface must be recreated, got %v", diags)
	}
	if len(s.calls("hostinterface.update")) != 0 {
		t.Fatal("the SNMP interface must never be touched")
	}
	var params map[string]interface{}
	_ = json.Unmarshal(s.calls("hostinterface.create")[0].Params, &params)
	if params["type"] != "1" || params["main"] != "1" || params["ip"] != "192.0.2.1" || params["hostid"] != "1" {
		t.Fatalf("agent interface must be created from the configuration, got %v", params)
	}
}

func TestActionRead_RefusesUnsupportedOperationType(t *testing.T) {
	// operationtype "1" (remote command) is not supported by the provider.
	c := fixtureServer(t, "action.get", strings.NewReplacer("%OP%", "1", "%DM%", "1").Replace(actionFixture))
	d := schema.TestResourceDataRaw(t, resourceAction().Schema, map[string]interface{}{})
	d.SetId("10")
	diags := resourceAction().ReadContext(context.Background(), d, c)
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "does not support") {
		t.Fatalf("unsupported operation type must be refused, got %v", diags)
	}
	if d.Id() != "10" {
		t.Error("ID must be kept so the action is not recreated")
	}
}
