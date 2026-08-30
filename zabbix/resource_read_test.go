package zabbix

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
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
		"parameters":[{"name":"a","value":"b"},{"name":"c","value":"d"}]}]`)
	d := readInto(t, resourceMediaType(), c, "46")

	if d.Get("smtp_server") != "" || d.Get("smtp_port") != 25 || d.Get("password") != "" || d.Get("smtp_authentication") != 0 {
		t.Errorf("email attributes must be reset for a webhook: %v %v %v", d.Get("smtp_server"), d.Get("smtp_port"), d.Get("password"))
	}
	if d.Get("script") != "return 1;" || d.Get("timeout") != "10s" {
		t.Errorf("webhook attributes not mapped")
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
		"exec_path":"","gsm_modem":"","script":"","timeout":"30s","parameters":[]}]`)
	d := readInto(t, resourceMediaType(), c, "45")
	want := map[string]interface{}{
		"enabled": false, "smtp_server": "mail.x", "smtp_port": 587, "smtp_security": 1,
		"smtp_verify_peer": true, "smtp_verify_host": false, "smtp_authentication": 1,
		"username": "u", "password": "secret", "script": "", "timeout": "30s",
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
	"pause_suppressed":"1","notify_if_canceled":"0",
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

	if d.Get("evaltype") != 2 || d.Get("pause_suppressed") != true || d.Get("notify_if_canceled") != false {
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
			return map[string][]string{"actionids": {"7"}}, nil
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
			return json.RawMessage(`[{"hostid":"1","host":"h","name":"h","status":"0","description":"",
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
			return json.RawMessage(`[{"hostid":"1","host":"snmp-only","name":"snmp-only","status":"0","description":"",
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
