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
	"time"
)

// ErrNotFound is returned by Get* methods when the API returned an empty result
// set. Note that Zabbix returns an empty set both when the object does not exist
// and when the current user has no permission to see it.
var ErrNotFound = errors.New("object not found")

// errNoAuth is returned when a request that requires authentication is attempted
// without a session token and no credentials are configured to obtain one.
var errNoAuth = errors.New("no authentication token available")

const (
	// sessionTerminated is the JSON-RPC error data Zabbix returns for an expired
	// or invalid session token.
	sessionTerminated = "Session terminated, re-login, please."
	// objectMissing is the JSON-RPC error data Zabbix returns when an operation
	// refers to an object that does not exist or is not visible to the user.
	objectMissing = "No permissions to referred object or it does not exist!"
)

// Version is set by main from -ldflags "-X main.version=..." and reported in
// the User-Agent header.
var Version = "dev"

type ClientConfig struct {
	URL        string
	Username   string
	Password   string
	APIToken   string
	Insecure   bool
	CACertFile string
	// Timeout bounds the wait for response headers of a single request. Zero
	// means no limit; the request context deadline applies in any case.
	Timeout time.Duration
}

type ZabbixClient struct {
	url        string
	username   string
	password   string
	apiToken   string
	httpClient *http.Client

	mu    sync.Mutex // guards token and flight
	token string
	// flight is the in-progress login, if any. Callers that observe the same
	// stale token wait for it (or for their own context) and share its result,
	// including a failure, instead of each logging in again.
	flight *loginFlight
}

type loginFlight struct {
	done chan struct{}
	err  error
}

type jsonRpcRequest struct {
	Jsonrpc string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
	ID      int         `json:"id"`
}

type jsonRpcResponse struct {
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

// IsObjectMissing reports whether err is a Zabbix API error stating that a
// referred object does not exist (or is not visible to the current user).
func IsObjectMissing(err error) bool {
	var rpcErr *JsonRpcError
	return errors.As(err, &rpcErr) && rpcErr.Data == objectMissing
}

func isSessionTerminated(err error) bool {
	var rpcErr *JsonRpcError
	return errors.As(err, &rpcErr) && rpcErr.Data == sessionTerminated
}

// --- API object models ---

type HostGroup struct {
	GroupID string `json:"groupid"`
	Name    string `json:"name"`
}

type Host struct {
	HostID          string          `json:"hostid"`
	Host            string          `json:"host"`
	Name            string          `json:"name"`
	Status          string          `json:"status"` // "0" = monitored, "1" = unmonitored
	Description     string          `json:"description"`
	Groups          []HostGroupRef  `json:"hostgroups"`
	ParentTemplates []TemplateRef   `json:"parentTemplates"`
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
	Type        string `json:"type"`  // "1" = Agent, "2" = SNMP, "3" = IPMI, "4" = JMX
	Main        string `json:"main"`  // "1" = default interface of its type
	UseIP       string `json:"useip"` // "1" = connect via IP, "0" = via DNS
	IP          string `json:"ip"`
	DNS         string `json:"dns"`
	Port        string `json:"port"`
}

// AgentInterface returns the main agent interface of the host, or nil.
func (h *Host) AgentInterface() *HostInterface {
	for i := range h.Interfaces {
		if h.Interfaces[i].Type == "1" && h.Interfaces[i].Main == "1" {
			return &h.Interfaces[i]
		}
	}
	return nil
}

type HostSpec struct {
	Host        string
	Name        string
	Status      string
	Description string
	GroupIDs    []string
	TemplateIDs []string
	Interface   HostInterface
}

type MediaType struct {
	// Parameters are only modelled for webhooks (name/value). For script media
	// types the API uses sortorder/value; Read refuses to manage those and
	// Update never sends them.
	MediaTypeID        string           `json:"mediatypeid,omitempty"`
	Name               string           `json:"name"`
	Type               string           `json:"type"`   // "0" = Email, "1" = Script, "2" = SMS, "4" = Webhook
	Status             string           `json:"status"` // "0" = enabled, "1" = disabled
	SMTPServer         string           `json:"smtp_server"`
	SMTPPort           string           `json:"smtp_port"`
	SMTPHelo           string           `json:"smtp_helo"`
	SMTPEmail          string           `json:"smtp_email"`
	SMTPSecurity       string           `json:"smtp_security"`
	SMTPVerifyPeer     string           `json:"smtp_verify_peer"`
	SMTPVerifyHost     string           `json:"smtp_verify_host"`
	SMTPAuthentication string           `json:"smtp_authentication"`
	Username           string           `json:"username"`
	Passwd             string           `json:"passwd"`
	ExecPath           string           `json:"exec_path"`
	GSMModem           string           `json:"gsm_modem"`
	Script             string           `json:"script"`
	Timeout            string           `json:"timeout"`
	Parameters         []MediaTypeParam `json:"parameters"`
}

type MediaTypeParam struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Action struct {
	ActionID         string            `json:"actionid,omitempty"`
	Name             string            `json:"name"`
	EventSource      string            `json:"eventsource"`
	Status           string            `json:"status"`
	EscPeriod        string            `json:"esc_period"`
	PauseSuppressed  string            `json:"pause_suppressed"`
	NotifyIfCanceled string            `json:"notify_if_canceled"`
	Filter           ActionFilter      `json:"filter"`
	Operations       []ActionOperation `json:"operations"`
}

type ActionFilter struct {
	EvalType   string            `json:"evaltype"`
	Conditions []ActionCondition `json:"conditions"`
}

type ActionCondition struct {
	ConditionType string `json:"conditiontype"`
	Operator      string `json:"operator"`
	Value         string `json:"value"`
	Value2        string `json:"value2,omitempty"` // tag name for "event tag value" conditions; rejected by the API for other types
}

type ActionOperation struct {
	OperationType string           `json:"operationtype"`
	EscPeriod     string           `json:"esc_period"`
	EscStepFrom   string           `json:"esc_step_from"`
	EscStepTo     string           `json:"esc_step_to"`
	OpMessage     *ActionOpMessage `json:"opmessage,omitempty"`
	// OpConditions are not modelled by the provider; they are read so that
	// Read can refuse to manage operations that have them (action.update
	// replaces operations wholesale and would drop them silently).
	OpConditions []json.RawMessage `json:"opconditions,omitempty"`
	// Always sent (possibly empty): Zabbix keeps existing recipients when the
	// field is omitted from action.update.
	OpMessageGrp []ActionOpMessageGrp `json:"opmessage_grp"`
	OpMessageUsr []ActionOpMessageUsr `json:"opmessage_usr"`
}

type ActionOpMessage struct {
	MediaTypeID string `json:"mediatypeid"`
	DefaultMsg  string `json:"default_msg"`
	Subject     string `json:"subject,omitempty"` // only allowed when DefaultMsg == "0"
	Message     string `json:"message,omitempty"` // only allowed when DefaultMsg == "0"
}

type ActionOpMessageGrp struct {
	Usrgrpid string `json:"usrgrpid"`
}

type ActionOpMessageUsr struct {
	UserID string `json:"userid"`
}

// --- Client ---

func NewZabbixClient(cfg ClientConfig) (*ZabbixClient, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.Insecure {
		tlsCfg.InsecureSkipVerify = true // #nosec G402 - explicit user opt-in
	}
	if cfg.CACertFile != "" {
		pem, err := os.ReadFile(cfg.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("reading ca_cert_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_cert_file %q contains no valid PEM certificates", cfg.CACertFile)
		}
		tlsCfg.RootCAs = pool
	}
	// No http.Client timeout: every call carries a context deadline (resource
	// timeouts for CRUD, an explicit timeout for provider configuration), so a
	// user-configured `timeouts { create = "15m" }` is honoured as-is.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsCfg
	transport.ResponseHeaderTimeout = cfg.Timeout // 0 = no limit

	return &ZabbixClient{
		url:      cfg.URL,
		username: cfg.Username,
		password: cfg.Password,
		apiToken: cfg.APIToken,
		token:    cfg.APIToken,
		httpClient: &http.Client{
			Transport: transport,
			// Never follow redirects: a 307/308 would replay the POST body and the
			// Authorization header, possibly to a downgraded http:// location.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// TLSConfig exposes the transport TLS configuration (used by tests).
func (c *ZabbixClient) TLSConfig() *tls.Config {
	return c.httpClient.Transport.(*http.Transport).TLSClientConfig
}

func (c *ZabbixClient) currentToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

// Call performs an authenticated JSON-RPC call. If the session has expired and
// the client was configured with username/password, it re-authenticates once
// (single-flight across goroutines) and retries. The retry is safe: Zabbix
// rejects the request before executing it when the session is invalid.
func (c *ZabbixClient) Call(ctx context.Context, method string, params interface{}, result interface{}) error {
	token := c.currentToken()
	if token == "" {
		if c.apiToken != "" || c.username == "" {
			return errNoAuth
		}
		if err := c.login(ctx, ""); err != nil {
			return err
		}
		token = c.currentToken()
	}

	err := c.rawCall(ctx, method, params, token, result)
	if err == nil || !isSessionTerminated(err) || c.apiToken != "" {
		return err
	}

	if err := c.login(ctx, token); err != nil {
		return err
	}
	return c.rawCall(ctx, method, params, c.currentToken(), result)
}

// login obtains a new session token. staleToken is the token that was observed
// to be invalid; if another goroutine already replaced it, login is skipped.
// Only one login runs at a time; other callers wait for its result or for
// their own context.
func (c *ZabbixClient) login(ctx context.Context, staleToken string) error {
	c.mu.Lock()
	if c.token != staleToken {
		c.mu.Unlock()
		return nil
	}
	if f := c.flight; f != nil {
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f := &loginFlight{done: make(chan struct{})}
	c.flight = f
	c.mu.Unlock()

	var token string
	params := map[string]string{"username": c.username, "password": c.password}
	err := c.rawCall(ctx, "user.login", params, "", &token)
	if err != nil {
		err = fmt.Errorf("user.login failed: %w", err)
	}

	// Publish the result and clear the flight atomically: a caller arriving in
	// between must either wait on this flight or see the new token.
	c.mu.Lock()
	if err == nil {
		c.token = token
	}
	f.err = err
	close(f.done)
	c.flight = nil
	c.mu.Unlock()
	return err
}

// Login authenticates eagerly. It is a no-op when an API token is configured.
func (c *ZabbixClient) Login(ctx context.Context) error {
	if c.apiToken != "" {
		return nil
	}
	return c.login(ctx, c.currentToken())
}

func (c *ZabbixClient) rawCall(ctx context.Context, method string, params interface{}, token string, result interface{}) error {
	reqBytes, err := json.Marshal(jsonRpcRequest{Jsonrpc: "2.0", Method: method, Params: params, ID: 1})
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(reqBytes))
	if err != nil {
		return fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json-rpc")
	req.Header.Set("User-Agent", "terraform-provider-zabbix/"+Version)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The body is deliberately not included: a misbehaving proxy could echo
		// the request (and its credentials) back into Terraform's diagnostics.
		return fmt.Errorf("unexpected http status %d (%s) from %s", resp.StatusCode, http.StatusText(resp.StatusCode), c.url)
	}

	var rpcResp jsonRpcResponse
	if err := json.Unmarshal(respBytes, &rpcResp); err != nil {
		return fmt.Errorf("failed to unmarshal response: %w", err)
	}
	if rpcResp.Error != nil {
		return rpcResp.Error
	}
	// A JSON-RPC 2.0 success response must carry a result. Anything else (an
	// HTML login page, an empty body, a proxy stub) must not be mistaken for a
	// successful mutation.
	if rpcResp.Jsonrpc != "2.0" || len(rpcResp.Result) == 0 || string(rpcResp.Result) == "null" {
		return fmt.Errorf("malformed JSON-RPC response from %s for %s: no result and no error", c.url, method)
	}
	if result != nil {
		if err := json.Unmarshal(rpcResp.Result, result); err != nil {
			return fmt.Errorf("failed to parse result: %w", err)
		}
	}
	return nil
}

// CheckAuth verifies that the configured credentials are accepted by making
// a cheap authenticated call.
func (c *ZabbixClient) CheckAuth(ctx context.Context) error {
	var res []map[string]string
	return c.Call(ctx, "user.get", map[string]interface{}{"output": []string{"userid"}, "limit": 1}, &res)
}

// GetVersion calls apiinfo.version (unauthenticated).
func (c *ZabbixClient) GetVersion(ctx context.Context) (string, error) {
	var version string
	if err := c.rawCall(ctx, "apiinfo.version", []interface{}{}, "", &version); err != nil {
		return "", err
	}
	return version, nil
}

func firstID(res map[string][]string, key string) (string, error) {
	ids := res[key]
	if len(ids) == 0 {
		return "", fmt.Errorf("no %s returned from Zabbix", key)
	}
	return ids[0], nil
}

// --- HOST GROUP ---

func (c *ZabbixClient) CreateHostGroup(ctx context.Context, name string) (string, error) {
	var res map[string][]string
	if err := c.Call(ctx, "hostgroup.create", map[string]string{"name": name}, &res); err != nil {
		return "", err
	}
	return firstID(res, "groupids")
}

func (c *ZabbixClient) GetHostGroup(ctx context.Context, id string) (*HostGroup, error) {
	params := map[string]interface{}{
		"groupids": []string{id},
		"output":   []string{"groupid", "name"},
	}
	var res []HostGroup
	if err := c.Call(ctx, "hostgroup.get", params, &res); err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, ErrNotFound
	}
	return &res[0], nil
}

func (c *ZabbixClient) UpdateHostGroup(ctx context.Context, id, name string) error {
	return c.Call(ctx, "hostgroup.update", map[string]string{"groupid": id, "name": name}, nil)
}

func (c *ZabbixClient) DeleteHostGroup(ctx context.Context, id string) error {
	return c.Call(ctx, "hostgroup.delete", []string{id}, nil)
}

// --- HOST ---

func groupRefs(ids []string) []HostGroupRef {
	refs := make([]HostGroupRef, len(ids))
	for i, id := range ids {
		refs[i] = HostGroupRef{GroupID: id}
	}
	return refs
}

func templateRefs(ids []string) []TemplateRef {
	refs := make([]TemplateRef, len(ids))
	for i, id := range ids {
		refs[i] = TemplateRef{TemplateID: id}
	}
	return refs
}

func hostParams(spec *HostSpec) map[string]interface{} {
	return map[string]interface{}{
		"host":        spec.Host,
		"name":        spec.Name,
		"status":      spec.Status,
		"description": spec.Description,
		"groups":      groupRefs(spec.GroupIDs),
		"templates":   templateRefs(spec.TemplateIDs),
	}
}

func (c *ZabbixClient) CreateHost(ctx context.Context, spec *HostSpec) (string, error) {
	params := hostParams(spec)
	iface := spec.Interface
	iface.Type, iface.Main = "1", "1"
	params["interfaces"] = []HostInterface{iface}

	var res map[string][]string
	if err := c.Call(ctx, "host.create", params, &res); err != nil {
		return "", err
	}
	return firstID(res, "hostids")
}

func (c *ZabbixClient) GetHost(ctx context.Context, id string) (*Host, error) {
	params := map[string]interface{}{
		"hostids":               []string{id},
		"output":                []string{"hostid", "host", "name", "status", "description"},
		"selectHostGroups":      []string{"groupid"},
		"selectParentTemplates": []string{"templateid"},
		"selectInterfaces":      []string{"interfaceid", "type", "main", "useip", "ip", "dns", "port"},
	}
	var res []Host
	if err := c.Call(ctx, "host.get", params, &res); err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, ErrNotFound
	}
	return &res[0], nil
}

// UpdateHost updates host properties, group and template links. Interfaces are
// intentionally NOT sent through host.update (which replaces the whole
// collection); use UpdateHostInterface for that. Templates present in
// templatesClear are unlinked and their inherited entities removed.
func (c *ZabbixClient) UpdateHost(ctx context.Context, id string, spec *HostSpec, templatesClear []string) error {
	params := hostParams(spec)
	params["hostid"] = id
	if len(templatesClear) > 0 {
		params["templates_clear"] = templateRefs(templatesClear)
	}
	return c.Call(ctx, "host.update", params, nil)
}

func (c *ZabbixClient) UpdateHostInterface(ctx context.Context, iface HostInterface) error {
	params := map[string]interface{}{
		"interfaceid": iface.InterfaceID,
		"useip":       iface.UseIP,
		"ip":          iface.IP,
		"dns":         iface.DNS,
		"port":        iface.Port,
	}
	return c.Call(ctx, "hostinterface.update", params, nil)
}

func (c *ZabbixClient) DeleteHost(ctx context.Context, id string) error {
	return c.Call(ctx, "host.delete", []string{id}, nil)
}

// --- MEDIA TYPE ---

// mediaTypeParams serialises the media type. Fields that do not belong to the
// media type's type are sent with their Zabbix defaults so that a type change
// clears leftovers (e.g. an SMTP password) instead of leaving them in Zabbix.
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
		delete(params, "parameters") // sortorder/value parameters are left untouched
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

func (c *ZabbixClient) CreateMediaType(ctx context.Context, mt *MediaType) (string, error) {
	var res map[string][]string
	if err := c.Call(ctx, "mediatype.create", mediaTypeParams(mt), &res); err != nil {
		return "", err
	}
	return firstID(res, "mediatypeids")
}

func (c *ZabbixClient) GetMediaType(ctx context.Context, id string) (*MediaType, error) {
	params := map[string]interface{}{
		"mediatypeids": []string{id},
		"output": []string{"mediatypeid", "name", "type", "status",
			"smtp_server", "smtp_port", "smtp_helo", "smtp_email", "smtp_security",
			"smtp_verify_peer", "smtp_verify_host", "smtp_authentication", "username", "passwd",
			"exec_path", "gsm_modem", "script", "timeout", "parameters"},
	}
	var res []MediaType
	if err := c.Call(ctx, "mediatype.get", params, &res); err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, ErrNotFound
	}
	return &res[0], nil
}

func (c *ZabbixClient) UpdateMediaType(ctx context.Context, mt *MediaType) error {
	params := mediaTypeParams(mt)
	params["mediatypeid"] = mt.MediaTypeID
	return c.Call(ctx, "mediatype.update", params, nil)
}

func (c *ZabbixClient) DeleteMediaType(ctx context.Context, id string) error {
	return c.Call(ctx, "mediatype.delete", []string{id}, nil)
}

// --- ACTION ---

func actionParams(action *Action) map[string]interface{} {
	ops := action.Operations
	if ops == nil {
		ops = []ActionOperation{}
	}
	conds := action.Filter.Conditions
	if conds == nil {
		conds = []ActionCondition{}
	}
	return map[string]interface{}{
		"name":               action.Name,
		"status":             action.Status,
		"esc_period":         action.EscPeriod,
		"pause_suppressed":   action.PauseSuppressed,
		"notify_if_canceled": action.NotifyIfCanceled,
		"filter":             ActionFilter{EvalType: action.Filter.EvalType, Conditions: conds},
		"operations":         ops,
	}
}

func (c *ZabbixClient) CreateAction(ctx context.Context, action *Action) (string, error) {
	params := actionParams(action)
	params["eventsource"] = action.EventSource
	var res map[string][]string
	if err := c.Call(ctx, "action.create", params, &res); err != nil {
		return "", err
	}
	return firstID(res, "actionids")
}

func (c *ZabbixClient) GetAction(ctx context.Context, id string) (*Action, error) {
	params := map[string]interface{}{
		"actionids":        []string{id},
		"output":           []string{"actionid", "name", "eventsource", "status", "esc_period", "pause_suppressed", "notify_if_canceled"},
		"selectFilter":     "extend",
		"selectOperations": "extend",
	}
	var res []Action
	if err := c.Call(ctx, "action.get", params, &res); err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, ErrNotFound
	}
	return &res[0], nil
}

// UpdateAction replaces filter and operations wholesale. eventsource is
// immutable in Zabbix and therefore never sent.
func (c *ZabbixClient) UpdateAction(ctx context.Context, action *Action) error {
	params := actionParams(action)
	params["actionid"] = action.ActionID
	return c.Call(ctx, "action.update", params, nil)
}

func (c *ZabbixClient) DeleteAction(ctx context.Context, id string) error {
	return c.Call(ctx, "action.delete", []string{id}, nil)
}
