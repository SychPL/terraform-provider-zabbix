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
}

type ZabbixClient struct {
	url        string
	username   string
	password   string
	apiToken   string
	httpClient *http.Client

	mu    sync.Mutex // guards token, flight and failed
	token string
	// flight is the in-progress login, if any. Callers that observe the same
	// stale token wait for it (or for their own context) and share its result,
	// including a failure, instead of each logging in again.
	flight *loginFlight
	// failed remembers the last failed login for a token generation so that
	// late callers holding the same stale token do not trigger another
	// attempt (and an account lockout) within loginFailureMemo.
	failed *loginFailure
}

type loginFlight struct {
	done chan struct{}
	err  error
}

type loginFailure struct {
	staleToken string
	err        error
	at         time.Time
}

const (
	// loginTimeout bounds a login performed on behalf of several callers; it is
	// detached from the initiating caller's context so that one short deadline
	// cannot cancel the login for everybody.
	loginTimeout = 60 * time.Second
	// loginFailureMemo is how long a failed login is returned to callers with
	// the same stale token instead of retrying.
	loginFailureMemo = 30 * time.Second
)

type jsonRpcRequest struct {
	Jsonrpc string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
	ID      int         `json:"id"`
}

type jsonRpcResponse struct {
	Jsonrpc string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	// Error is kept raw so that the PRESENCE of the member can be checked:
	// "error": null would otherwise decode to nil and hide a spec violation.
	Error json.RawMessage `json:"error,omitempty"`
	ID    int             `json:"id"`
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
	Flags           string          `json:"flags"`  // "0" = plain, "4" = discovered by LLD
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
	Description        string           `json:"description"`
	MaxSessions        string           `json:"maxsessions"`      // parallel alert sessions; SMS supports only "1"
	MaxAttempts        string           `json:"maxattempts"`      // delivery attempts, 1-100
	AttemptInterval    string           `json:"attempt_interval"` // 0-1h with time suffix
	ContentType        string           `json:"content_type"`     // Email: "0" plain text, "1" HTML
	ProcessTags        string           `json:"process_tags"`     // Webhook: "1" = response processed as tags
	ShowEventMenu      string           `json:"show_event_menu"`  // Webhook: "1" = event menu entry
	EventMenuURL       string           `json:"event_menu_url"`   // Webhook
	EventMenuName      string           `json:"event_menu_name"`  // Webhook
	// ClearParameters forces an empty parameter list even for script media
	// types; set on a type change so webhook parameters do not linger.
	ClearParameters bool `json:"-"`
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
	PauseSymptoms    string            `json:"pause_symptoms"` // "1" pauses escalation for symptom problems (6.4+)
	NotifyIfCanceled string            `json:"notify_if_canceled"`
	Filter           ActionFilter      `json:"filter"`
	Operations       []ActionOperation `json:"operations"`
	// Recovery/update operations are not modelled; they are read only so that
	// Read can refuse actions carrying them (they cannot be round-tripped and
	// would silently stay unmanaged after an import).
	RecoveryOperations []json.RawMessage `json:"recovery_operations,omitempty"`
	UpdateOperations   []json.RawMessage `json:"update_operations,omitempty"`
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
	// Only allowed when DefaultMsg == "0" (the API rejects them otherwise),
	// but then always sent, even when empty: action.update merges omitted
	// fields with the stored values, so an omitted subject would silently
	// keep a stale one forever (perpetual diff).
	Subject *string `json:"subject,omitempty"`
	Message *string `json:"message,omitempty"`
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
		// Extra CAs are added to the system pool, not used instead of it.
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
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
	if lf := c.failed; lf != nil && lf.staleToken == staleToken && time.Since(lf.at) < loginFailureMemo {
		c.mu.Unlock()
		return lf.err
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

	// The login itself is detached from the initiating caller (its own
	// deadline) so that other waiters still get a token if the initiator gives
	// up; the initiator waits for it like everybody else, honouring its ctx.
	go func() {
		loginCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loginTimeout)
		defer cancel()
		var token string
		params := map[string]string{"username": c.username, "password": c.password}
		err := c.rawCall(loginCtx, "user.login", params, "", &token)
		if err == nil && token == "" {
			// A successful-looking login without a session must never publish
			// an empty token: every later request would silently go out
			// without an Authorization header.
			err = errors.New("empty session token returned")
		}
		if err != nil {
			err = fmt.Errorf("user.login failed: %w", err)
		}

		// Publish the result and clear the flight atomically: a caller arriving
		// in between must either wait on this flight or see the new token.
		c.mu.Lock()
		if err == nil {
			c.token = token
			c.failed = nil
		} else {
			c.failed = &loginFailure{staleToken: staleToken, err: err, at: time.Now()}
		}
		f.err = err
		close(f.done)
		c.flight = nil
		c.mu.Unlock()
	}()

	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
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

	req, err := newSingleShotRequest(ctx, c.url, reqBytes, token)
	if err != nil {
		return fmt.Errorf("failed to build request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// The body is deliberately not read or included: a misbehaving proxy
		// could echo the request (and its credentials) back into Terraform's
		// diagnostics, and reading it first would allow forcing a large
		// allocation on every failing call.
		return fmt.Errorf("unexpected http status %d (%s) from %s", resp.StatusCode, http.StatusText(resp.StatusCode), c.url)
	}
	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20+1))
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}
	if len(respBytes) > 32<<20 {
		// An explicit error beats the misleading JSON parse failure a silent
		// truncation would produce.
		return fmt.Errorf("response from %s exceeds 32 MiB; refusing to parse a truncated body", c.url)
	}

	var rpcResp jsonRpcResponse
	if err := json.Unmarshal(respBytes, &rpcResp); err != nil {
		return fmt.Errorf("failed to unmarshal response: %w", err)
	}
	// The envelope is validated before the error is even classified: a
	// response that is not JSON-RPC 2.0 with our request ID must not be able
	// to steer the client (e.g. fake a session expiry to force a re-login and
	// a retried mutation).
	if rpcResp.Jsonrpc != "2.0" || rpcResp.ID != 1 {
		return fmt.Errorf("malformed JSON-RPC response from %s for %s: unexpected envelope", c.url, method)
	}
	if len(rpcResp.Error) != 0 {
		if len(rpcResp.Result) != 0 {
			// JSON-RPC 2.0 forbids carrying both - even "error": null. Do not
			// let such a response steer error handling (e.g. fake a session
			// expiry after a success) or count as a successful mutation.
			return fmt.Errorf("malformed JSON-RPC response from %s for %s: both result and error", c.url, method)
		}
		var rpcErr JsonRpcError
		if string(rpcResp.Error) == "null" || json.Unmarshal(rpcResp.Error, &rpcErr) != nil {
			return fmt.Errorf("malformed JSON-RPC response from %s for %s: unparsable error member", c.url, method)
		}
		return &rpcErr
	}
	// A success response must carry a result. Anything else (an HTML login
	// page, an empty body, a proxy stub) must not be mistaken for a successful
	// mutation.
	if len(rpcResp.Result) == 0 || string(rpcResp.Result) == "null" {
		return fmt.Errorf("malformed JSON-RPC response from %s for %s: no result and no error", c.url, method)
	}
	if result != nil {
		if err := json.Unmarshal(rpcResp.Result, result); err != nil {
			return fmt.Errorf("failed to parse result: %w", err)
		}
	}
	return nil
}

// CheckAuth verifies that the configured API token is valid.
// user.checkAuthentication is used because it does not depend on the token's
// role having access to any particular API method; the Zabbix API requires it
// to be called without an Authorization header.
func (c *ZabbixClient) CheckAuth(ctx context.Context) error {
	var user map[string]interface{}
	return c.rawCall(ctx, "user.checkAuthentication", map[string]string{"token": c.apiToken}, "", &user)
}

// GetVersion calls apiinfo.version (unauthenticated).
func (c *ZabbixClient) GetVersion(ctx context.Context) (string, error) {
	var version string
	if err := c.rawCall(ctx, "apiinfo.version", []interface{}{}, "", &version); err != nil {
		return "", err
	}
	return version, nil
}

// newSingleShotRequest builds a request that net/http can never transparently
// replay: GetBody is removed, because a replayed create/update/delete on a
// dying reused connection would execute the mutation twice.
func newSingleShotRequest(ctx context.Context, url string, body []byte, token string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json-rpc")
	req.Header.Set("User-Agent", "terraform-provider-zabbix/"+Version)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}

func firstID(res map[string][]string, key string) (string, error) {
	ids := res[key]
	if len(ids) == 0 || ids[0] == "" {
		return "", fmt.Errorf("no %s returned from Zabbix", key)
	}
	return ids[0], nil
}

// mutate performs a mutation and verifies that the response confirms the
// mutated object: {"result": false}, an empty ID list, empty IDs or a foreign
// object's ID must never count as success. id may be empty for calls that
// create an object (the returned ID is unknown up front).
func (c *ZabbixClient) mutate(ctx context.Context, method string, params interface{}, key, id string) error {
	var res map[string][]string
	if err := c.Call(ctx, method, params, &res); err != nil {
		return err
	}
	ids := res[key]
	if len(ids) == 0 {
		return fmt.Errorf("%s reported success but returned no %s", method, key)
	}
	if id == "" {
		for _, got := range ids {
			if got != "" {
				return nil
			}
		}
		return fmt.Errorf("%s reported success but returned only empty %s", method, key)
	}
	for _, got := range ids {
		if got == id {
			return nil
		}
	}
	return fmt.Errorf("%s reported success but returned %s %q instead of %q", method, key, ids, id)
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
	return c.mutate(ctx, "hostgroup.update", map[string]string{"groupid": id, "name": name}, "groupids", id)
}

func (c *ZabbixClient) DeleteHostGroup(ctx context.Context, id string) error {
	return c.mutate(ctx, "hostgroup.delete", []string{id}, "groupids", id)
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

// HasAddress reports whether the interface describes a reachable endpoint.
func (i HostInterface) HasAddress() bool {
	return i.IP != "" || i.DNS != ""
}

func (c *ZabbixClient) CreateHost(ctx context.Context, spec *HostSpec) (string, error) {
	params := hostParams(spec)
	// A host without an address is created without any interface (valid in
	// Zabbix, e.g. for hosts monitored through dependent or trapper items).
	if spec.Interface.HasAddress() {
		iface := spec.Interface
		iface.Type, iface.Main = "1", "1"
		params["interfaces"] = []HostInterface{iface}
	}

	var res map[string][]string
	if err := c.Call(ctx, "host.create", params, &res); err != nil {
		return "", err
	}
	return firstID(res, "hostids")
}

func (c *ZabbixClient) GetHost(ctx context.Context, id string) (*Host, error) {
	params := map[string]interface{}{
		"hostids":               []string{id},
		"output":                []string{"hostid", "host", "name", "status", "description", "flags"},
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
	return c.mutate(ctx, "host.update", params, "hostids", id)
}

// CreateHostInterface adds a main agent interface to an existing host.
func (c *ZabbixClient) CreateHostInterface(ctx context.Context, hostID string, iface HostInterface) error {
	params := map[string]interface{}{
		"hostid": hostID,
		"type":   "1",
		"main":   "1",
		"useip":  iface.UseIP,
		"ip":     iface.IP,
		"dns":    iface.DNS,
		"port":   iface.Port,
	}
	return c.mutate(ctx, "hostinterface.create", params, "interfaceids", "")
}

// DeleteHostInterface removes an interface from a host.
func (c *ZabbixClient) DeleteHostInterface(ctx context.Context, interfaceID string) error {
	return c.mutate(ctx, "hostinterface.delete", []string{interfaceID}, "interfaceids", interfaceID)
}

func (c *ZabbixClient) UpdateHostInterface(ctx context.Context, iface HostInterface) error {
	params := map[string]interface{}{
		"interfaceid": iface.InterfaceID,
		"useip":       iface.UseIP,
		"ip":          iface.IP,
		"dns":         iface.DNS,
		"port":        iface.Port,
	}
	return c.mutate(ctx, "hostinterface.update", params, "interfaceids", iface.InterfaceID)
}

func (c *ZabbixClient) DeleteHost(ctx context.Context, id string) error {
	return c.mutate(ctx, "host.delete", []string{id}, "hostids", id)
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
		// Common to every transport, always taken from the configuration.
		"description":      mt.Description,
		"maxsessions":      mt.MaxSessions,
		"maxattempts":      mt.MaxAttempts,
		"attempt_interval": mt.AttemptInterval,
		// Type-specific fields reset to API defaults unless the type sets them.
		"content_type":    "1",
		"process_tags":    "0",
		"show_event_menu": "0",
		"event_menu_url":  "",
		"event_menu_name": "",
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
		params["content_type"] = mt.ContentType
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
		params["process_tags"] = mt.ProcessTags
		params["show_event_menu"] = mt.ShowEventMenu
		params["event_menu_url"] = mt.EventMenuURL
		params["event_menu_name"] = mt.EventMenuName
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
			"exec_path", "gsm_modem", "script", "timeout", "parameters",
			"description", "maxsessions", "maxattempts", "attempt_interval",
			"content_type", "process_tags", "show_event_menu", "event_menu_url", "event_menu_name"},
	}
	var raw []json.RawMessage
	if err := c.Call(ctx, "mediatype.get", params, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, ErrNotFound
	}
	// Since Zabbix 6.4.19 non-Super-Admin roles receive only a restricted
	// field set (mediatypeid, name, type, status, maxattempts). Treating the
	// missing fields as empty would reset the real configuration on the next
	// refresh or import, so a restricted response is a hard error instead.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw[0], &probe); err != nil {
		return nil, fmt.Errorf("failed to parse result: %w", err)
	}
	// Any of these type-independent fields marks a full response; a server
	// that ever omits fields foreign to the media type's transport still
	// returns description/maxsessions, so a full read is never mistaken for
	// a restricted one (which carries none of the three).
	full := false
	for _, f := range []string{"smtp_server", "description", "maxsessions"} {
		if _, ok := probe[f]; ok {
			full = true
			break
		}
	}
	if !full {
		return nil, fmt.Errorf("mediatype.get returned a restricted field set; managing media types requires a Super Admin role (since Zabbix 6.4.19 other roles cannot read media type details)")
	}
	var mt MediaType
	if err := json.Unmarshal(raw[0], &mt); err != nil {
		return nil, fmt.Errorf("failed to parse result: %w", err)
	}
	return &mt, nil
}

func (c *ZabbixClient) UpdateMediaType(ctx context.Context, mt *MediaType) error {
	params := mediaTypeParams(mt)
	params["mediatypeid"] = mt.MediaTypeID
	return c.mutate(ctx, "mediatype.update", params, "mediatypeids", mt.MediaTypeID)
}

func (c *ZabbixClient) DeleteMediaType(ctx context.Context, id string) error {
	return c.mutate(ctx, "mediatype.delete", []string{id}, "mediatypeids", id)
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
		"pause_symptoms":     action.PauseSymptoms,
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
		"output":           []string{"actionid", "name", "eventsource", "status", "esc_period", "pause_suppressed", "pause_symptoms", "notify_if_canceled"},
		"selectFilter":     "extend",
		"selectOperations": "extend",
		// Read only for the refusal check in flattenAction.
		"selectRecoveryOperations": "extend",
		"selectUpdateOperations":   "extend",
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
	return c.mutate(ctx, "action.update", params, "actionids", action.ActionID)
}

func (c *ZabbixClient) DeleteAction(ctx context.Context, id string) error {
	return c.mutate(ctx, "action.delete", []string{id}, "actionids", id)
}
