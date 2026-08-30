package zabbix

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

type ZabbixClient struct {
	URL        string
	Username   string
	Password   string
	AuthToken  string
	HTTPClient *http.Client
}

type JsonRpcRequest struct {
	Jsonrpc string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
	ID      int         `json:"id"`
	Auth    interface{} `json:"auth,omitempty"`
}

type JsonRpcResponse struct {
	Jsonrpc string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JsonRpcError   `json:"error,omitempty"`
	ID      int             `json:"id"`
}

type JsonRpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

func (e *JsonRpcError) Error() string {
	return fmt.Sprintf("Zabbix API Error %d: %s (%s)", e.Code, e.Message, e.Data)
}

type HostGroup struct {
	GroupID string `json:"groupid"`
	Name    string `json:"name"`
}

type Host struct {
	HostID     string           `json:"hostid"`
	Host       string           `json:"host"`
	Groups     []HostGroupRef   `json:"groups"`
	ParentTemplates []TemplateRef `json:"parentTemplates,omitempty"`
	Interfaces []HostInterface  `json:"interfaces"`
}

type HostGroupRef struct {
	GroupID string `json:"groupid"`
}

type TemplateRef struct {
	TemplateID string `json:"templateid"`
}

type HostInterface struct {
	InterfaceID string `json:"interfaceid,omitempty"`
	Type        string `json:"type"`  // "1" = Agent, "2" = SNMP, "3" = IPMI, "4" = JMX
	Main        string `json:"main"`  // "1" = default
	UseIP       string `json:"useip"` // "1" = IP, "0" = DNS
	IP          string `json:"ip"`
	DNS         string `json:"dns"`
	Port        string `json:"port"`
}

type MediaType struct {
	MediaTypeID string           `json:"mediatypeid,omitempty"`
	Name        string           `json:"name"`
	Type        string           `json:"type"` // "0" = Email, "1" = Script, "2" = SMS, "4" = Webhook
	Status      string           `json:"status"` // "0" = enabled, "1" = disabled
	SMTPServer  string           `json:"smtp_server,omitempty"`
	SMTPHelo    string           `json:"smtp_helo,omitempty"`
	SMTPEmail   string           `json:"smtp_email,omitempty"`
	ExecPath    string           `json:"exec_path,omitempty"`
	GSMModem    string           `json:"gsm_modem,omitempty"`
	Script      string           `json:"script,omitempty"`
	Timeout     string           `json:"timeout,omitempty"`
	Parameters  []MediaTypeParam `json:"parameters,omitempty"`
}

type MediaTypeParam struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Action struct {
	ActionID    string            `json:"actionid,omitempty"`
	Name        string            `json:"name"`
	EventSource string            `json:"eventsource"` // string
	Status      string            `json:"status"`      // string
	EscPeriod   string            `json:"esc_period,omitempty"`
	Filter      ActionFilter      `json:"filter"`
	Operations  []ActionOperation `json:"operations,omitempty"`
}

type ActionFilter struct {
	EvalType   string            `json:"evaltype"` // string
	Conditions []ActionCondition `json:"conditions"`
}

type ActionCondition struct {
	ConditionType string `json:"conditiontype"` // string
	Operator      string `json:"operator"`      // string
	Value         string `json:"value"`
}

type ActionOperation struct {
	OperationType string                 `json:"operationtype"` // string
	EscPeriod     string                 `json:"esc_period,omitempty"`
	EscStepFrom   string                 `json:"esc_step_from,omitempty"`
	EscStepTo     string                 `json:"esc_step_to,omitempty"`
	OpMessage     *ActionOpMessage       `json:"opmessage,omitempty"`
	OpMessageGrp  []ActionOpMessageGrp   `json:"opmessage_grp,omitempty"`
}

type ActionOpMessage struct {
	MediaTypeID string `json:"mediatypeid,omitempty"`
	DefaultMsg  string `json:"default_msg"` // string
	Subject     string `json:"subject,omitempty"`
	Message     string `json:"message,omitempty"`
}

type ActionOpMessageGrp struct {
	Usrgrpid string `json:"usrgrpid"`
}

func NewZabbixClient(url, username, password string) *ZabbixClient {
	return &ZabbixClient{
		URL:      url,
		Username: username,
		Password: password,
		HTTPClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

func (c *ZabbixClient) Call(method string, params interface{}, result interface{}) error {
	reqID := 1
	reqPayload := JsonRpcRequest{
		Jsonrpc: "2.0",
		Method:  method,
		Params:  params,
		ID:      reqID,
	}

	// Attach Auth Token if logged in
	if c.AuthToken != "" && method != "user.login" && method != "apiinfo.version" {
		reqPayload.Auth = c.AuthToken
	}

	reqBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := c.HTTPClient.Post(c.URL, "application/json-rpc", bytes.NewBuffer(reqBytes))
	if err != nil {
		return fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected http status code: %d", resp.StatusCode)
	}

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	var rpcResp JsonRpcResponse
	if err := json.Unmarshal(respBytes, &rpcResp); err != nil {
		return fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if rpcResp.Error != nil {
		return rpcResp.Error
	}

	if result != nil {
		if err := json.Unmarshal(rpcResp.Result, result); err != nil {
			return fmt.Errorf("failed to parse result: %w", err)
		}
	}

	return nil
}

func (c *ZabbixClient) Login() error {
	params := map[string]string{
		"username": c.Username,
		"password": c.Password,
	}

	var token string
	err := c.Call("user.login", params, &token)
	if err != nil {
		return err
	}

	c.AuthToken = token
	return nil
}

// --- HOST GROUP CRUD ---

func (c *ZabbixClient) CreateHostGroup(name string) (string, error) {
	params := map[string]string{
		"name": name,
	}

	var res map[string][]string
	err := c.Call("hostgroup.create", params, &res)
	if err != nil {
		return "", err
	}

	ids, ok := res["groupids"]
	if !ok || len(ids) == 0 {
		return "", fmt.Errorf("no groupids returned from Zabbix")
	}

	return ids[0], nil
}

func (c *ZabbixClient) GetHostGroup(id string) (*HostGroup, error) {
	params := map[string]interface{}{
		"groupids": []string{id},
		"output":   []string{"groupid", "name"},
	}

	var res []HostGroup
	err := c.Call("hostgroup.get", params, &res)
	if err != nil {
		return nil, err
	}

	if len(res) == 0 {
		return nil, fmt.Errorf("host group %s not found", id)
	}

	return &res[0], nil
}

func (c *ZabbixClient) UpdateHostGroup(id string, name string) error {
	params := map[string]string{
		"groupid": id,
		"name":    name,
	}

	var res map[string][]string
	return c.Call("hostgroup.update", params, &res)
}

func (c *ZabbixClient) DeleteHostGroup(id string) error {
	params := []string{id}
	var res map[string][]string
	return c.Call("hostgroup.delete", params, &res)
}

// --- HOST CRUD ---

func (c *ZabbixClient) CreateHost(name string, groupIds []string, templateIds []string, ip, port string) (string, error) {
	groups := make([]HostGroupRef, len(groupIds))
	for i, id := range groupIds {
		groups[i] = HostGroupRef{GroupID: id}
	}

	templates := make([]TemplateRef, len(templateIds))
	for i, id := range templateIds {
		templates[i] = TemplateRef{TemplateID: id}
	}

	interfaces := []HostInterface{
		{
			Type:  "1", // Agent
			Main:  "1", // Default interface
			UseIP: "1", // Use IP
			IP:    ip,
			DNS:   "",
			Port:  port,
		},
	}

	params := map[string]interface{}{
		"host":       name,
		"groups":     groups,
		"interfaces": interfaces,
	}

	if len(templates) > 0 {
		params["templates"] = templates
	}

	var res map[string][]string
	err := c.Call("host.create", params, &res)
	if err != nil {
		return "", err
	}

	ids, ok := res["hostids"]
	if !ok || len(ids) == 0 {
		return "", fmt.Errorf("no hostids returned from Zabbix")
	}

	return ids[0], nil
}

func (c *ZabbixClient) GetHost(id string) (*Host, error) {
	params := map[string]interface{}{
		"hostids":          []string{id},
		"output":           []string{"hostid", "host"},
		"selectGroups":     []string{"groupid"},
		"selectParentTemplates": []string{"templateid"},
		"selectInterfaces": []string{"interfaceid", "ip", "port"},
	}

	var res []Host
	err := c.Call("host.get", params, &res)
	if err != nil {
		return nil, err
	}

	if len(res) == 0 {
		return nil, fmt.Errorf("host %s not found", id)
	}

	return &res[0], nil
}

func (c *ZabbixClient) UpdateHost(id string, name string, groupIds []string, templateIds []string, ip, port string) error {
	groups := make([]HostGroupRef, len(groupIds))
	for i, gid := range groupIds {
		groups[i] = HostGroupRef{GroupID: gid}
	}

	templates := make([]TemplateRef, len(templateIds))
	for i, tid := range templateIds {
		templates[i] = TemplateRef{TemplateID: tid}
	}

	// In Zabbix, updating interfaces is tricky. Usually we want to update the main agent interface.
	// First we fetch the existing host's interfaces to get the interface ID.
	existing, err := c.GetHost(id)
	if err != nil {
		return err
	}

	interfaces := []HostInterface{
		{
			Type:  "1",
			Main:  "1",
			UseIP: "1",
			IP:    ip,
			DNS:   "",
			Port:  port,
		},
	}

	// Reuse existing interfaceid if possible
	if len(existing.Interfaces) > 0 {
		interfaces[0].InterfaceID = existing.Interfaces[0].InterfaceID
	}

	params := map[string]interface{}{
		"hostid":     id,
		"host":       name,
		"groups":     groups,
		"interfaces": interfaces,
	}

	if len(templates) > 0 {
		params["templates"] = templates
	} else {
		// Clear templates by sending empty array if none defined
		params["templates"] = []TemplateRef{}
	}

	var res map[string][]string
	return c.Call("host.update", params, &res)
}

func (c *ZabbixClient) DeleteHost(id string) error {
	params := []string{id}
	var res map[string][]string
	return c.Call("host.delete", params, &res)
}

func (c *ZabbixClient) GetVersion() (string, error) {
	var version string
	err := c.Call("apiinfo.version", map[string]interface{}{}, &version)
	if err != nil {
		return "", err
	}
	return version, nil
}

// --- MEDIA TYPE CRUD ---

func (c *ZabbixClient) CreateMediaType(mediaType *MediaType) (string, error) {
	params := map[string]interface{}{
		"name":   mediaType.Name,
		"type":   mediaType.Type,
		"status": mediaType.Status,
	}

	if mediaType.SMTPServer != "" {
		params["smtp_server"] = mediaType.SMTPServer
	}
	if mediaType.SMTPHelo != "" {
		params["smtp_helo"] = mediaType.SMTPHelo
	}
	if mediaType.SMTPEmail != "" {
		params["smtp_email"] = mediaType.SMTPEmail
	}
	if mediaType.ExecPath != "" {
		params["exec_path"] = mediaType.ExecPath
	}
	if mediaType.GSMModem != "" {
		params["gsm_modem"] = mediaType.GSMModem
	}
	if mediaType.Script != "" {
		params["script"] = mediaType.Script
	}
	if mediaType.Timeout != "" {
		params["timeout"] = mediaType.Timeout
	}
	if len(mediaType.Parameters) > 0 {
		params["parameters"] = mediaType.Parameters
	}

	var res map[string][]string
	err := c.Call("mediatype.create", params, &res)
	if err != nil {
		return "", err
	}

	ids, ok := res["mediatypeids"]
	if !ok || len(ids) == 0 {
		return "", fmt.Errorf("no mediatypeids returned from Zabbix")
	}

	return ids[0], nil
}

func (c *ZabbixClient) GetMediaType(id string) (*MediaType, error) {
	params := map[string]interface{}{
		"mediatypeids":     []string{id},
		"output":           []string{"mediatypeid", "name", "type", "status", "smtp_server", "smtp_helo", "smtp_email", "exec_path", "gsm_modem", "script", "timeout"},
		"selectParameters": "extend",
	}

	var res []MediaType
	err := c.Call("mediatype.get", params, &res)
	if err != nil {
		return nil, err
	}

	if len(res) == 0 {
		return nil, fmt.Errorf("media type %s not found", id)
	}

	return &res[0], nil
}

func (c *ZabbixClient) UpdateMediaType(mediaType *MediaType) error {
	params := map[string]interface{}{
		"mediatypeid": mediaType.MediaTypeID,
		"name":        mediaType.Name,
		"type":        mediaType.Type,
		"status":      mediaType.Status,
	}

	if mediaType.SMTPServer != "" {
		params["smtp_server"] = mediaType.SMTPServer
	}
	if mediaType.SMTPHelo != "" {
		params["smtp_helo"] = mediaType.SMTPHelo
	}
	if mediaType.SMTPEmail != "" {
		params["smtp_email"] = mediaType.SMTPEmail
	}
	if mediaType.ExecPath != "" {
		params["exec_path"] = mediaType.ExecPath
	}
	if mediaType.GSMModem != "" {
		params["gsm_modem"] = mediaType.GSMModem
	}
	if mediaType.Script != "" {
		params["script"] = mediaType.Script
	}
	if mediaType.Timeout != "" {
		params["timeout"] = mediaType.Timeout
	}
	// In Zabbix 6.4, updating webhook parameters overwrites them
	if mediaType.Type == "4" {
		params["parameters"] = mediaType.Parameters
	} else {
		// Non-webhooks should have empty/no parameters passed
		params["parameters"] = []interface{}{}
	}

	var res map[string][]string
	return c.Call("mediatype.update", params, &res)
}

func (c *ZabbixClient) DeleteMediaType(id string) error {
	params := []string{id}
	var res map[string][]string
	return c.Call("mediatype.delete", params, &res)
}

// --- ACTION CRUD ---

func (c *ZabbixClient) CreateAction(action *Action) (string, error) {
	// Build base parameters
	params := map[string]interface{}{
		"name":        action.Name,
		"eventsource": action.EventSource,
		"status":      action.Status,
		"filter":      action.Filter,
		"operations":  action.Operations,
	}

	if action.EscPeriod != "" {
		params["esc_period"] = action.EscPeriod
	}

	// Make sure operations is at least an empty array if empty
	if len(action.Operations) == 0 {
		params["operations"] = []interface{}{}
	}

	debugBytes, _ := json.Marshal(params)
	fmt.Fprintf(os.Stderr, "[DEBUG ZABBIX] action.create params: %s\n", string(debugBytes))

	var res map[string][]string
	err := c.Call("action.create", params, &res)
	if err != nil {
		return "", err
	}

	ids, ok := res["actionids"]
	if !ok || len(ids) == 0 {
		return "", fmt.Errorf("no actionids returned from Zabbix")
	}

	return ids[0], nil
}

func (c *ZabbixClient) GetAction(id string) (*Action, error) {
	params := map[string]interface{}{
		"actionids":    []string{id},
		"output":       []string{"actionid", "name", "eventsource", "status", "esc_period"},
		"selectFilter": "extend",
		"selectOperations": "extend",
	}

	var res []Action
	err := c.Call("action.get", params, &res)
	if err != nil {
		return nil, err
	}

	if len(res) == 0 {
		return nil, fmt.Errorf("action %s not found", id)
	}

	return &res[0], nil
}

func (c *ZabbixClient) UpdateAction(action *Action) error {
	params := map[string]interface{}{
		"actionid":    action.ActionID,
		"name":        action.Name,
		"eventsource": action.EventSource,
		"status":      action.Status,
		"filter":      action.Filter,
		"operations":  action.Operations,
	}

	if action.EscPeriod != "" {
		params["esc_period"] = action.EscPeriod
	}

	// Make sure operations is at least an empty array if empty
	if len(action.Operations) == 0 {
		params["operations"] = []interface{}{}
	}

	var res map[string][]string
	return c.Call("action.update", params, &res)
}

func (c *ZabbixClient) DeleteAction(id string) error {
	params := []string{id}
	var res map[string][]string
	return c.Call("action.delete", params, &res)
}
