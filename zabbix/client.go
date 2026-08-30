package zabbix

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	if c.AuthToken != "" && method != "user.login" {
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
