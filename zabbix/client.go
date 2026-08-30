package zabbix

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
)

var ErrNotFound = fmt.Errorf("resource not found")

const objectMissing = "No permissions to referred object or it does not exist!"

func IsObjectMissing(err error) bool {
	var rpcErr *JsonRpcError
	return errors.As(err, &rpcErr) && rpcErr.Data == objectMissing
}

type ZabbixClient struct {
	URL          string
	Username     string
	Password     string
	APIToken     string
	SessionToken string
	TLSInsecure  bool
	CACertFile   string
	HTTPClient   *http.Client
	loginMu      sync.Mutex
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
	HostID          string          `json:"hostid"`
	Host            string          `json:"host"`
	Groups          []HostGroupRef  `json:"groups"`
	ParentTemplates []TemplateRef   `json:"parentTemplates,omitempty"`
	Interfaces      []HostInterface `json:"interfaces"`
}

type HostGroupRef struct {
	GroupID string `json:"groupid"`
}

type TemplateRef struct {
	TemplateID string `json:"templateid"`
}

type HostInterface struct {
	InterfaceID string `json:"interfaceid,omitempty"`
	HostID      string `json:"hostid,omitempty"`
	Type        string `json:"type"`  // "1" = Agent, "2" = SNMP, "3" = IPMI, "4" = JMX
	Main        string `json:"main"`  // "1" = default
	UseIP       string `json:"useip"` // "1" = IP, "0" = DNS
	IP          string `json:"ip"`
	DNS         string `json:"dns"`
	Port        string `json:"port"`
}

type MediaType struct {
	MediaTypeID        string           `json:"mediatypeid,omitempty"`
	Name               string           `json:"name"`
	Type               string           `json:"type"`   // "0" = Email, "1" = Script, "2" = SMS, "4" = Webhook
	Status             string           `json:"status"` // "0" = enabled, "1" = disabled
	SMTPServer         string           `json:"smtp_server,omitempty"`
	SMTPPort           string           `json:"smtp_port,omitempty"`
	SMTPHelo           string           `json:"smtp_helo,omitempty"`
	SMTPEmail          string           `json:"smtp_email,omitempty"`
	SMTPSecurity       string           `json:"smtp_security,omitempty"`
	SMTPVerifyPeer     string           `json:"smtp_verify_peer,omitempty"`
	SMTPVerifyHost     string           `json:"smtp_verify_host,omitempty"`
	SMTPAuthentication string           `json:"smtp_authentication,omitempty"`
	Username           string           `json:"username,omitempty"`
	Passwd             string           `json:"passwd,omitempty"`
	ExecPath           string           `json:"exec_path,omitempty"`
	GSMModem           string           `json:"gsm_modem,omitempty"`
	Script             string           `json:"script,omitempty"`
	Timeout            string           `json:"timeout,omitempty"`
	Parameters         []MediaTypeParam `json:"parameters,omitempty"`
	ClearParameters    bool             `json:"-"`
}

type MediaTypeParam struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Action struct {
	ActionID    string            `json:"actionid,omitempty"`
	Name        string            `json:"name"`
	EventSource string            `json:"eventsource"` // "0" = Triggers
	Status      string            `json:"status"`      // "0" = Enabled, "1" = Disabled
	EscPeriod   string            `json:"esc_period,omitempty"`
	Filter      ActionFilter      `json:"filter"`
	Operations  []ActionOperation `json:"operations,omitempty"`
}

type ActionFilter struct {
	EvalType   string            `json:"evaltype"` // "0" = AND/OR
	Conditions []ActionCondition `json:"conditions"`
}

type ActionCondition struct {
	ConditionType string `json:"conditiontype"`
	Operator      string `json:"operator"`
	Value         string `json:"value"`
}

type ActionOperation struct {
	OperationType string               `json:"operationtype"` // "0" = send message
	EscPeriod     string               `json:"esc_period,omitempty"`
	EscStepFrom   string               `json:"esc_step_from,omitempty"`
	EscStepTo     string               `json:"esc_step_to,omitempty"`
	OpMessage     *ActionOpMessage     `json:"opmessage,omitempty"`
	OpMessageGrp  []ActionOpMessageGrp `json:"opmessage_grp,omitempty"`
}

type ActionOpMessage struct {
	MediaTypeID string `json:"mediatypeid,omitempty"`
	DefaultMsg  string `json:"default_msg"` // "1" = yes, "0" = no
	Subject     string `json:"subject,omitempty"`
	Message     string `json:"message,omitempty"`
}

type ActionOpMessageGrp struct {
	Usrgrpid string `json:"usrgrpid"`
}

func NewZabbixClient(url, username, password, apiToken string, tlsInsecure bool, caCertFile string) (*ZabbixClient, error) {
	client := &ZabbixClient{
		URL:         url,
		Username:    username,
		Password:    password,
		APIToken:    apiToken,
		TLSInsecure: tlsInsecure,
		CACertFile:  caCertFile,
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tlsConfig := &tls.Config{}

	if tlsInsecure {
		tlsConfig.InsecureSkipVerify = true
	}

	if caCertFile != "" {
		caCert, err := os.ReadFile(caCertFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA cert file: %w", err)
		}
		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA cert PEM")
		}
		tlsConfig.RootCAs = caCertPool
	}

	tr.TLSClientConfig = tlsConfig

	client.HTTPClient = &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return client, nil
}

func (c *ZabbixClient) TLSConfig() *tls.Config {
	return c.HTTPClient.Transport.(*http.Transport).TLSClientConfig
}

func (c *ZabbixClient) prepareRequest(ctx context.Context, method string, reqBytes []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", c.URL, bytes.NewBuffer(reqBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json-rpc")

	if method != "apiinfo.version" {
		if c.APIToken != "" {
			req.Header.Set("Authorization", "Bearer "+c.APIToken)
		} else if c.SessionToken != "" {
			req.Header.Set("Authorization", "Bearer "+c.SessionToken)
		}
	}
	return req, nil
}

func (c *ZabbixClient) ensureLogin(ctx context.Context) error {
	if c.APIToken != "" {
		return nil
	}

	c.loginMu.Lock()
	defer c.loginMu.Unlock()

	if c.SessionToken != "" {
		return nil
	}

	params := map[string]string{
		"username": c.Username,
		"password": c.Password,
	}

	var token string
	err := c.rawCall(ctx, "user.login", params, &token)
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}

	c.SessionToken = token
	return nil
}

func (c *ZabbixClient) rawCall(ctx context.Context, method string, params interface{}, result interface{}) error {
	reqID := 1
	reqPayload := JsonRpcRequest{
		Jsonrpc: "2.0",
		Method:  method,
		Params:  params,
		ID:      reqID,
	}

	reqBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := c.prepareRequest(ctx, method, reqBytes)
	if err != nil {
		return fmt.Errorf("failed to prepare request: %w", err)
	}

	resp, err := c.HTTPClient.Do(req)
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

	if rpcResp.Jsonrpc != "2.0" || rpcResp.Result == nil {
		return fmt.Errorf("invalid json-rpc response")
	}

	if result != nil {
		if err := json.Unmarshal(rpcResp.Result, result); err != nil {
			return fmt.Errorf("failed to parse result: %w", err)
		}
	}

	return nil
}

func (c *ZabbixClient) Call(ctx context.Context, method string, params interface{}, result interface{}) error {
	if method != "apiinfo.version" && method != "user.login" {
		if err := c.ensureLogin(ctx); err != nil {
			return err
		}
	}

	err := c.rawCall(ctx, method, params, result)
	if err != nil {
		if method != "apiinfo.version" && method != "user.login" && isSessionExpiredError(err) {
			c.loginMu.Lock()
			c.SessionToken = ""
			c.loginMu.Unlock()

			if errLogin := c.ensureLogin(ctx); errLogin != nil {
				return errLogin
			}

			return c.rawCall(ctx, method, params, result)
		}
		return err
	}

	return nil
}

func isSessionExpiredError(err error) bool {
	if rpcErr, ok := err.(*JsonRpcError); ok {
		return rpcErr.Code == -32602 && (rpcErr.Message == "Session terminated, re-login, please." || rpcErr.Message == "Session expired.")
	}
	return false
}

func (c *ZabbixClient) Login(ctx context.Context) error {
	return c.ensureLogin(ctx)
}

// --- HOST GROUP CRUD ---

func (c *ZabbixClient) CreateHostGroup(ctx context.Context, name string) (string, error) {
	params := map[string]string{
		"name": name,
	}

	var res map[string][]string
	err := c.Call(ctx, "hostgroup.create", params, &res)
	if err != nil {
		return "", err
	}

	ids, ok := res["groupids"]
	if !ok || len(ids) == 0 {
		return "", fmt.Errorf("no groupids returned from Zabbix")
	}

	return ids[0], nil
}

func (c *ZabbixClient) GetHostGroup(ctx context.Context, id string) (*HostGroup, error) {
	params := map[string]interface{}{
		"groupids": []string{id},
		"output":   []string{"groupid", "name"},
	}

	var res []HostGroup
	err := c.Call(ctx, "hostgroup.get", params, &res)
	if err != nil {
		return nil, err
	}

	if len(res) == 0 {
		return nil, ErrNotFound
	}

	return &res[0], nil
}

func (c *ZabbixClient) UpdateHostGroup(ctx context.Context, id string, name string) error {
	params := map[string]string{
		"groupid": id,
		"name":    name,
	}

	var res map[string][]string
	return c.Call(ctx, "hostgroup.update", params, &res)
}

func (c *ZabbixClient) DeleteHostGroup(ctx context.Context, id string) error {
	params := []string{id}
	var res map[string][]string
	return c.Call(ctx, "hostgroup.delete", params, &res)
}

// --- HOST CRUD ---

func (c *ZabbixClient) CreateHost(ctx context.Context, name string, groupIds []string, templateIds []string, useIP, ip, dns, port string) (string, error) {
	groups := make([]HostGroupRef, len(groupIds))
	for i, id := range groupIds {
		groups[i] = HostGroupRef{GroupID: id}
	}

	templates := make([]TemplateRef, len(templateIds))
	for i, id := range templateIds {
		templates[i] = TemplateRef{TemplateID: id}
	}

	params := map[string]interface{}{
		"host":   name,
		"groups": groups,
	}

	if ip != "" || dns != "" {
		interfaces := []HostInterface{
			{
				Type:  "1", // Agent
				Main:  "1", // Default interface
				UseIP: useIP,
				IP:    ip,
				DNS:   dns,
				Port:  port,
			},
		}
		params["interfaces"] = interfaces
	}

	if len(templates) > 0 {
		params["templates"] = templates
	}

	var res map[string][]string
	err := c.Call(ctx, "host.create", params, &res)
	if err != nil {
		return "", err
	}

	ids, ok := res["hostids"]
	if !ok || len(ids) == 0 {
		return "", fmt.Errorf("no hostids returned from Zabbix")
	}

	return ids[0], nil
}

func (c *ZabbixClient) GetHost(ctx context.Context, id string) (*Host, error) {
	params := map[string]interface{}{
		"hostids":               []string{id},
		"output":                []string{"hostid", "host"},
		"selectGroups":          []string{"groupid"},
		"selectParentTemplates": []string{"templateid"},
		"selectInterfaces":      []string{"interfaceid", "type", "main", "useip", "ip", "dns", "port"},
	}

	var res []Host
	err := c.Call(ctx, "host.get", params, &res)
	if err != nil {
		return nil, err
	}

	if len(res) == 0 {
		return nil, ErrNotFound
	}

	return &res[0], nil
}

func (c *ZabbixClient) UpdateHost(ctx context.Context, id string, name string, groupIds []string, templateIds []string, templatesClearIds []string) error {
	groups := make([]HostGroupRef, len(groupIds))
	for i, gid := range groupIds {
		groups[i] = HostGroupRef{GroupID: gid}
	}

	templates := make([]TemplateRef, len(templateIds))
	for i, tid := range templateIds {
		templates[i] = TemplateRef{TemplateID: tid}
	}

	params := map[string]interface{}{
		"hostid": id,
		"host":   name,
		"groups": groups,
	}

	if len(templates) > 0 {
		params["templates"] = templates
	} else {
		params["templates"] = []TemplateRef{}
	}

	if len(templatesClearIds) > 0 {
		clear := make([]TemplateRef, len(templatesClearIds))
		for i, cid := range templatesClearIds {
			clear[i] = TemplateRef{TemplateID: cid}
		}
		params["templates_clear"] = clear
	}

	var res map[string][]string
	return c.Call(ctx, "host.update", params, &res)
}

func (c *ZabbixClient) GetHostInterface(ctx context.Context, hostID string) (*HostInterface, error) {
	params := map[string]interface{}{
		"hostids": []string{hostID},
		"output":  []string{"interfaceid", "type", "main", "useip", "ip", "dns", "port"},
	}
	var res []HostInterface
	err := c.Call(ctx, "hostinterface.get", params, &res)
	if err != nil {
		return nil, err
	}
	for _, inter := range res {
		if inter.Type == "1" && inter.Main == "1" {
			return &inter, nil
		}
	}
	return nil, ErrNotFound
}

func (c *ZabbixClient) CreateHostInterface(ctx context.Context, hostID string, useIP, ip, dns, port string) error {
	params := map[string]interface{}{
		"hostid": hostID,
		"type":   "1",
		"main":   "1",
		"useip":  useIP,
		"ip":     ip,
		"dns":    dns,
		"port":   port,
	}
	var res map[string][]string
	return c.Call(ctx, "hostinterface.create", params, &res)
}

func (c *ZabbixClient) DeleteHostInterface(ctx context.Context, id string) error {
	params := []string{id}
	var res map[string][]string
	return c.Call(ctx, "hostinterface.delete", params, &res)
}

func (c *ZabbixClient) UpdateHostInterface(ctx context.Context, inter *HostInterface) error {
	var res map[string][]string
	return c.Call(ctx, "hostinterface.update", inter, &res)
}

func (c *ZabbixClient) DeleteHost(ctx context.Context, id string) error {
	params := []string{id}
	var res map[string][]string
	return c.Call(ctx, "host.delete", params, &res)
}

func (c *ZabbixClient) GetVersion(ctx context.Context) (string, error) {
	var version string
	err := c.Call(ctx, "apiinfo.version", map[string]interface{}{}, &version)
	if err != nil {
		return "", err
	}
	return version, nil
}

// --- MEDIA TYPE CRUD ---

func mediaTypeParams(mt *MediaType) map[string]interface{} {
	params := map[string]interface{}{
		"name":                mt.Name,
		"type":                mt.Type,
		"status":              mt.Status,
		"smtp_server":         "",
		"smtp_port":           "25",
		"smtp_helo":           "",
		"smtp_email":          "",
		"smtp_security":       "0",
		"smtp_verify_peer":    "0",
		"smtp_verify_host":    "0",
		"smtp_authentication": "0",
		"username":            "",
		"passwd":              "",
		"exec_path":           "",
		"gsm_modem":           "",
		"script":              "",
		"timeout":             "30s",
		"parameters":          []MediaTypeParam{},
	}
	switch mt.Type {
	case "0": // Email
		params["smtp_server"] = mt.SMTPServer
		params["smtp_port"] = mt.SMTPPort
		params["smtp_helo"] = mt.SMTPHelo
		params["smtp_email"] = mt.SMTPEmail
		params["smtp_security"] = mt.SMTPSecurity
		params["smtp_verify_peer"] = mt.SMTPVerifyPeer
		params["smtp_verify_host"] = mt.SMTPVerifyHost
		params["smtp_authentication"] = mt.SMTPAuthentication
		if mt.SMTPAuthentication == "1" {
			params["username"] = mt.Username
			params["passwd"] = mt.Passwd
		}
	case "1": // Script
		params["exec_path"] = mt.ExecPath
		if !mt.ClearParameters {
			delete(params, "parameters") // sortorder/value parameters are left untouched
		}
	case "2": // SMS
		params["gsm_modem"] = mt.GSMModem
	case "4": // Webhook
		params["script"] = mt.Script
		params["timeout"] = mt.Timeout
		if mt.Parameters != nil {
			params["parameters"] = mt.Parameters
		}
	}
	return params
}

func (c *ZabbixClient) CreateMediaType(ctx context.Context, mediaType *MediaType) (string, error) {
	var res map[string][]string
	err := c.Call(ctx, "mediatype.create", mediaTypeParams(mediaType), &res)
	if err != nil {
		return "", err
	}

	ids, ok := res["mediatypeids"]
	if !ok || len(ids) == 0 {
		return "", fmt.Errorf("no mediatypeids returned from Zabbix")
	}

	return ids[0], nil
}

func (c *ZabbixClient) GetMediaType(ctx context.Context, id string) (*MediaType, error) {
	params := map[string]interface{}{
		"mediatypeids":     []string{id},
		"output":           []string{"mediatypeid", "name", "type", "status", "smtp_server", "smtp_helo", "smtp_email", "exec_path", "gsm_modem", "script", "timeout", "smtp_port", "smtp_security", "smtp_verify_peer", "smtp_verify_host", "smtp_authentication", "username", "passwd"},
		"selectParameters": "extend",
	}

	var res []MediaType
	err := c.Call(ctx, "mediatype.get", params, &res)
	if err != nil {
		return nil, err
	}

	if len(res) == 0 {
		return nil, ErrNotFound
	}

	return &res[0], nil
}

func (c *ZabbixClient) UpdateMediaType(ctx context.Context, mediaType *MediaType) error {
	params := mediaTypeParams(mediaType)
	params["mediatypeid"] = mediaType.MediaTypeID

	var res map[string][]string
	return c.Call(ctx, "mediatype.update", params, &res)
}

func (c *ZabbixClient) DeleteMediaType(ctx context.Context, id string) error {
	params := []string{id}
	var res map[string][]string
	return c.Call(ctx, "mediatype.delete", params, &res)
}

// --- ACTION CRUD ---

func (c *ZabbixClient) CreateAction(ctx context.Context, action *Action) (string, error) {
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

	if len(action.Operations) == 0 {
		params["operations"] = []interface{}{}
	}

	var res map[string][]string
	err := c.Call(ctx, "action.create", params, &res)
	if err != nil {
		return "", err
	}

	ids, ok := res["actionids"]
	if !ok || len(ids) == 0 {
		return "", fmt.Errorf("no actionids returned from Zabbix")
	}

	return ids[0], nil
}

func (c *ZabbixClient) GetAction(ctx context.Context, id string) (*Action, error) {
	params := map[string]interface{}{
		"actionids":        []string{id},
		"output":           []string{"actionid", "name", "eventsource", "status", "esc_period"},
		"selectFilter":     "extend",
		"selectOperations": "extend",
	}

	var res []Action
	err := c.Call(ctx, "action.get", params, &res)
	if err != nil {
		return nil, err
	}

	if len(res) == 0 {
		return nil, ErrNotFound
	}

	return &res[0], nil
}

func (c *ZabbixClient) UpdateAction(ctx context.Context, action *Action) error {
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

	if len(action.Operations) == 0 {
		params["operations"] = []interface{}{}
	}

	var res map[string][]string
	return c.Call(ctx, "action.update", params, &res)
}

func (c *ZabbixClient) DeleteAction(ctx context.Context, id string) error {
	params := []string{id}
	var res map[string][]string
	return c.Call(ctx, "action.delete", params, &res)
}
