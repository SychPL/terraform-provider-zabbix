package zabbix

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// rpcServer is a minimal JSON-RPC 2.0 mock of the Zabbix API.
type rpcServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []rpcRequest
	handler  func(req rpcRequest) (result interface{}, rpcErr *JsonRpcError)
}

type rpcRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Auth   string          `json:"-"`
}

func newRPCServer(t *testing.T, handler func(req rpcRequest) (interface{}, *JsonRpcError)) *rpcServer {
	t.Helper()
	s := &rpcServer{handler: handler}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		req.Auth = r.Header.Get("Authorization")
		s.mu.Lock()
		s.requests = append(s.requests, req)
		s.mu.Unlock()

		result, rpcErr := s.handler(req)
		resp := map[string]interface{}{"jsonrpc": "2.0", "id": 1}
		if rpcErr != nil {
			resp["error"] = rpcErr
		} else {
			resp["result"] = result
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *rpcServer) calls(method string) []rpcRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []rpcRequest
	for _, r := range s.requests {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

func newTestClient(t *testing.T, s *rpcServer, cfg ClientConfig) *ZabbixClient {
	t.Helper()
	cfg.URL = s.URL
	c, err := NewZabbixClient(cfg)
	if err != nil {
		t.Fatalf("NewZabbixClient: %v", err)
	}
	return c
}

var passwordCfg = ClientConfig{Username: "Admin", Password: "zabbix"}

func TestGetHostGroup_NotFoundVsError(t *testing.T) {
	var fail atomic.Bool
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "user.login":
			return "tok", nil
		case "hostgroup.get":
			if fail.Load() {
				return nil, &JsonRpcError{Code: -32500, Message: "Application error.", Data: "boom"}
			}
			return []HostGroup{}, nil
		}
		return nil, nil
	})
	c := newTestClient(t, s, passwordCfg)

	_, err := c.GetHostGroup(context.Background(), "1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty result: want ErrNotFound, got %v", err)
	}

	fail.Store(true)
	_, err = c.GetHostGroup(context.Background(), "1")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("API error: must not be ErrNotFound, got %v", err)
	}
}

func TestCall_HTTPErrorIsNotNotFound(t *testing.T) {
	const echoedSecret = "Authorization: Bearer LEAKED-TOKEN-7f3a"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A misbehaving proxy echoing the request back.
		http.Error(w, "gateway down\n"+echoedSecret, http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	c, _ := NewZabbixClient(ClientConfig{URL: srv.URL, APIToken: "tok"})

	_, err := c.GetHost(context.Background(), "1")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("want transport error, got %v", err)
	}
	if strings.Contains(err.Error(), "LEAKED-TOKEN") {
		t.Fatalf("response body must not be included in diagnostics: %v", err)
	}
}

func TestCall_MutationIsNeverRetriedOnTransportError(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		// Execute the create and drop the connection without a response.
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	t.Cleanup(srv.Close)
	c, _ := NewZabbixClient(ClientConfig{URL: srv.URL, APIToken: "tok"})

	if _, err := c.CreateHostGroup(context.Background(), "g"); err == nil {
		t.Fatal("dropped connection must be an error")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("a mutation must be sent exactly once, got %d requests", got)
	}
}

func TestCall_BearerHeaderAndUnauthenticatedVersion(t *testing.T) {
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		if req.Method == "apiinfo.version" {
			return "6.4.21", nil
		}
		return []HostGroup{{GroupID: "1", Name: "g"}}, nil
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "secret-token"})

	if _, err := c.GetVersion(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetHostGroup(context.Background(), "1"); err != nil {
		t.Fatal(err)
	}

	if got := s.calls("apiinfo.version")[0]; got.Auth != "" || string(got.Params) != "[]" {
		t.Errorf("apiinfo.version: want no auth and [] params, got auth=%q params=%s", got.Auth, got.Params)
	}
	if got := s.calls("hostgroup.get")[0].Auth; got != "Bearer secret-token" {
		t.Errorf("want Bearer header, got %q", got)
	}
	if len(s.calls("user.login")) != 0 {
		t.Error("api token auth must not call user.login")
	}
}

func TestCall_ReloginOnceOnSessionExpiry(t *testing.T) {
	const parallel = 5
	var logins atomic.Int32
	// Barrier: every parallel call must see the expired session before any of
	// them gets the error, so that they all race for the re-login.
	var barrier sync.WaitGroup
	barrier.Add(parallel)
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "user.login":
			n := logins.Add(1)
			return map[int32]string{1: "tok1", 2: "tok2"}[n], nil
		case "hostgroup.get":
			if req.Auth != "Bearer tok2" {
				barrier.Done()
				barrier.Wait()
				return nil, &JsonRpcError{Code: -32602, Message: "Invalid params.", Data: sessionTerminated}
			}
			return []HostGroup{{GroupID: "1", Name: "g"}}, nil
		}
		return nil, nil
	})
	c := newTestClient(t, s, passwordCfg)
	if err := c.Login(context.Background()); err != nil {
		t.Fatal(err)
	}

	// All parallel calls hit an expired session: exactly one re-login must happen.
	var wg sync.WaitGroup
	errs := make(chan error, parallel)
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.GetHostGroup(context.Background(), "1")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("call after re-login failed: %v", err)
		}
	}
	if got := logins.Load(); got != 2 {
		t.Fatalf("want initial login + exactly one re-login (2), got %d", got)
	}
}

func TestCall_FailedReloginIsSharedByWaiters(t *testing.T) {
	const parallel = 5
	var logins atomic.Int32
	var barrier sync.WaitGroup
	barrier.Add(parallel)
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "user.login":
			if logins.Add(1) == 1 {
				return "tok1", nil
			}
			return nil, &JsonRpcError{Code: -32602, Message: "Invalid params.", Data: "Incorrect user name or password or account is temporarily blocked."}
		default:
			barrier.Done()
			barrier.Wait()
			return nil, &JsonRpcError{Code: -32602, Message: "Invalid params.", Data: sessionTerminated}
		}
	})
	c := newTestClient(t, s, passwordCfg)
	if err := c.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.GetHostGroup(context.Background(), "1"); err == nil || !strings.Contains(err.Error(), "user.login failed") {
				t.Errorf("want shared login failure, got %v", err)
			}
		}()
	}
	wg.Wait()
	if got := logins.Load(); got != 2 {
		t.Fatalf("a failed re-login must be shared: want 1 initial + 1 failed login, got %d", got)
	}
}

func TestMediaTypeParams_ScriptParametersUntouched(t *testing.T) {
	p := mediaTypeParams(&MediaType{Type: "1", ExecPath: "x.sh"})
	if _, ok := p["parameters"]; ok {
		t.Error("script media types must not send parameters (sortorder/value are not modelled)")
	}
	// On a type change the leftover webhook parameters must be cleared.
	p = mediaTypeParams(&MediaType{Type: "1", ExecPath: "x.sh", ClearParameters: true})
	if params, ok := p["parameters"].([]MediaTypeParam); !ok || len(params) != 0 {
		t.Errorf("type change to script must send an empty parameter list, got %#v", p["parameters"])
	}
}

func TestCall_FailedLoginIsMemoisedForLateCallers(t *testing.T) {
	var logins atomic.Int32
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "user.login":
			if logins.Add(1) == 1 {
				return "tok1", nil
			}
			return nil, &JsonRpcError{Code: -32602, Message: "Invalid params.", Data: "Incorrect user name or password or account is temporarily blocked."}
		default:
			return nil, &JsonRpcError{Code: -32602, Message: "Invalid params.", Data: sessionTerminated}
		}
	})
	c := newTestClient(t, s, passwordCfg)
	if err := c.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Sequential (late) callers holding the same expired token must not each
	// trigger a new login attempt.
	for i := 0; i < 4; i++ {
		if _, err := c.GetHostGroup(context.Background(), "1"); err == nil || !strings.Contains(err.Error(), "user.login failed") {
			t.Fatalf("call %d: want memoised login failure, got %v", i, err)
		}
	}
	if got := logins.Load(); got != 2 {
		t.Fatalf("want 1 initial + 1 failed login, got %d", got)
	}
}

func TestCall_ReloginSurvivesInitiatorCancellation(t *testing.T) {
	var logins atomic.Int32
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "user.login":
			if logins.Add(1) == 1 {
				return "tok1", nil // initial session, expires immediately below
			}
			time.Sleep(500 * time.Millisecond) // the slow re-login
			return "tok2", nil
		case "hostgroup.get":
			if req.Auth != "Bearer tok2" {
				return nil, &JsonRpcError{Code: -32602, Message: "Invalid params.", Data: sessionTerminated}
			}
			return []HostGroup{{GroupID: "1", Name: "g"}}, nil
		}
		return "tok1", nil
	})
	c := newTestClient(t, s, passwordCfg)
	if err := c.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The initiator gives up quickly (and must not be held for the whole
	// login); a patient caller must still get the new token.
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.GetHostGroup(short, "1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("initiator must return with its own deadline error, got %v", err)
	}
	// The login sleeps 500 ms; returning well before that proves the initiator
	// did not wait for it (generous margin for a loaded CI runner).
	if time.Since(start) > 300*time.Millisecond {
		t.Fatalf("initiator waited for the full login instead of honouring its context (%s)", time.Since(start))
	}
	if _, err := c.GetHostGroup(context.Background(), "1"); err != nil {
		t.Fatalf("the login must complete for other callers even if its initiator was cancelled: %v", err)
	}
}

func TestCall_LazyFirstLoginIsSingleFlight(t *testing.T) {
	var logins atomic.Int32
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		if req.Method == "user.login" {
			logins.Add(1)
			time.Sleep(50 * time.Millisecond)
			return "tok", nil
		}
		return []HostGroup{{GroupID: "1", Name: "g"}}, nil
	})
	c := newTestClient(t, s, passwordCfg) // no eager Login

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.GetHostGroup(context.Background(), "1"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := logins.Load(); got != 1 {
		t.Fatalf("want exactly one lazy login, got %d", got)
	}
}

func TestCall_RedirectsAreNotFollowed(t *testing.T) {
	var followed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed.Store(true)
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	c, _ := NewZabbixClient(ClientConfig{URL: srv.URL + "/api_jsonrpc.php", APIToken: "secret"})

	_, err := c.GetHostGroup(context.Background(), "1")
	if err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("redirect must be reported as an error, got %v", err)
	}
	if followed.Load() {
		t.Fatal("the redirect target must never be requested (would replay the token and body)")
	}
}

func TestCall_ForgedErrorEnvelopeDoesNotTriggerRelogin(t *testing.T) {
	// An injected non-JSON-RPC body must not be able to fake a session expiry
	// (which would re-login and retry an already executed mutation).
	var logins, deletes atomic.Int32
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		if req.Method == "user.login" {
			logins.Add(1)
			return "tok", nil
		}
		deletes.Add(1)
		return nil, nil // handled below by overriding the body
	})
	_ = s
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deletes.Add(1)
		// Correct error data but a broken envelope (wrong version, wrong id).
		_, _ = w.Write([]byte(`{"jsonrpc":"1.0","error":{"code":-32602,"message":"Invalid params.","data":"` + sessionTerminated + `"},"id":7}`))
	}))
	t.Cleanup(srv.Close)
	c, _ := NewZabbixClient(ClientConfig{URL: srv.URL, Username: "u", Password: "p"})
	c.token = "tok"

	err := c.DeleteHost(context.Background(), "1")
	if err == nil || !strings.Contains(err.Error(), "unexpected envelope") {
		t.Fatalf("broken envelope must be a malformed-response error, got %v", err)
	}
	if deletes.Load() != 1 {
		t.Fatalf("the mutation must not be retried, got %d requests", deletes.Load())
	}
}

func TestRawCall_RequestsAreNotReplayable(t *testing.T) {
	// The exact request constructor used by rawCall must produce requests that
	// net/http can never transparently replay.
	req, err := newSingleShotRequest(context.Background(), "http://example.test/api", []byte(`{"x":1}`), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if req.GetBody != nil {
		t.Fatal("GetBody must be nil, otherwise net/http may replay a mutation on a dying reused connection")
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("unexpected auth header %q", got)
	}
	// Sanity: the stdlib does set GetBody for a bytes.Reader by default, so the
	// constructor really removes something.
	plain, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.test", bytes.NewReader([]byte("x")))
	if plain.GetBody == nil {
		t.Skip("stdlib no longer sets GetBody for known body types")
	}
}

func TestCall_MalformedSuccessResponse(t *testing.T) {
	for _, body := range []string{`{}`, `{"jsonrpc":"2.0","id":1}`, `{"jsonrpc":"2.0","result":null,"id":1}`, `{"jsonrpc":"2.0","result":{"hostids":["1"]},"id":2}`, `{"result":{"hostids":["1"]},"id":1}`,
		`{"jsonrpc":"2.0","result":{"hostids":["1"]},"error":{"code":-32602,"message":"Invalid params.","data":"Session terminated, re-login, please."},"id":1}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		c, _ := NewZabbixClient(ClientConfig{URL: srv.URL, APIToken: "t"})
		if err := c.DeleteHost(context.Background(), "1"); err == nil {
			t.Errorf("body %s: a response without a JSON-RPC 2.0 result must not be treated as success", body)
		}
		srv.Close()
	}
}

func TestCall_NoReloginWithAPIToken(t *testing.T) {
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		return nil, &JsonRpcError{Code: -32602, Message: "Invalid params.", Data: sessionTerminated}
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "bad"})

	_, err := c.GetHostGroup(context.Background(), "1")
	if !isSessionTerminated(err) {
		t.Fatalf("want session error surfaced, got %v", err)
	}
	if n := len(s.requests); n != 1 {
		t.Fatalf("want exactly 1 request (no retry), got %d", n)
	}
}

func TestCall_ContextCancelled(t *testing.T) {
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		time.Sleep(200 * time.Millisecond)
		return "tok", nil
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "tok"})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := c.GetHostGroup(ctx, "1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context deadline error, got %v", err)
	}
}

func TestNewZabbixClient_NoImplicitTimeout(t *testing.T) {
	c, _ := NewZabbixClient(ClientConfig{URL: "https://x", APIToken: "t"})
	if c.httpClient.Timeout != 0 {
		t.Fatalf("the http client must not cap requests; resource timeouts do (got %s)", c.httpClient.Timeout)
	}
}

func TestNewZabbixClient_TLS(t *testing.T) {
	c, err := NewZabbixClient(ClientConfig{URL: "https://x", APIToken: "t", Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	if !c.TLSConfig().InsecureSkipVerify {
		t.Error("Insecure must set InsecureSkipVerify")
	}

	if _, err := NewZabbixClient(ClientConfig{URL: "https://x", APIToken: "t", CACertFile: filepath.Join(t.TempDir(), "missing.pem")}); err == nil {
		t.Error("missing ca_cert_file must fail")
	}

	bad := filepath.Join(t.TempDir(), "bad.pem")
	_ = os.WriteFile(bad, []byte("not a pem"), 0o600)
	if _, err := NewZabbixClient(ClientConfig{URL: "https://x", APIToken: "t", CACertFile: bad}); err == nil {
		t.Error("invalid PEM must fail")
	}

	// A self-signed TLS server is trusted once its certificate is given as CA.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":"6.4.21","id":1}`))
	}))
	t.Cleanup(srv.Close)
	good := filepath.Join(t.TempDir(), "ca.pem")
	_ = os.WriteFile(good, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)

	c, err = NewZabbixClient(ClientConfig{URL: srv.URL, APIToken: "t", CACertFile: good})
	if err != nil {
		t.Fatal(err)
	}
	if c.TLSConfig().RootCAs == nil {
		t.Error("ca_cert_file must set RootCAs")
	}
	// The custom CA is added to the system pool, not used instead of it.
	if sys, err := x509.SystemCertPool(); err == nil && sys != nil && c.TLSConfig().RootCAs.Equal(x509.NewCertPool()) {
		t.Error("RootCAs must extend the system pool, not be empty")
	}
	if _, err := c.GetVersion(context.Background()); err != nil {
		t.Errorf("request with custom CA failed: %v", err)
	}

	untrusted, _ := NewZabbixClient(ClientConfig{URL: srv.URL, APIToken: "t"})
	if _, err := untrusted.GetVersion(context.Background()); err == nil {
		t.Error("self-signed certificate must be rejected without ca_cert_file")
	}
}

func TestMediaTypeParams_TypeAware(t *testing.T) {
	wh := mediaTypeParams(&MediaType{Type: "4", Script: "return 1;", Timeout: "30s", SMTPServer: "stale", Passwd: "stale"})
	if p, ok := wh["parameters"].([]MediaTypeParam); !ok || p == nil {
		t.Errorf("webhook without parameters must send an empty array, got %#v", wh["parameters"])
	}
	// Fields of other types are cleared, never carried over.
	if wh["smtp_server"] != "" || wh["passwd"] != "" || wh["smtp_port"] != "25" {
		t.Errorf("webhook must clear email fields, got smtp_server=%v passwd=%v", wh["smtp_server"], wh["passwd"])
	}

	email := mediaTypeParams(&MediaType{Type: "0", SMTPServer: "mail", SMTPAuthentication: "0", Username: "u", Passwd: "p", Script: "stale"})
	if email["passwd"] != "" || email["username"] != "" {
		t.Error("credentials must not be sent when smtp_authentication is 0")
	}
	if email["script"] != "" || email["smtp_server"] != "mail" {
		t.Error("email must clear webhook fields and send smtp fields")
	}
	emailAuth := mediaTypeParams(&MediaType{Type: "0", SMTPAuthentication: "1", Username: "u", Passwd: "p"})
	if emailAuth["passwd"] != "p" {
		t.Error("passwd must be sent when smtp_authentication is 1")
	}
}

func TestActionParams_EventSourceOnlyOnCreate(t *testing.T) {
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		return map[string][]string{"actionids": {"7"}}, nil
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})
	a := &Action{ActionID: "7", Name: "a", EventSource: "0"}

	if _, err := c.CreateAction(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if err := c.UpdateAction(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	var create, update map[string]interface{}
	_ = json.Unmarshal(s.calls("action.create")[0].Params, &create)
	_ = json.Unmarshal(s.calls("action.update")[0].Params, &update)
	if create["eventsource"] != "0" {
		t.Error("create must send eventsource")
	}
	if _, ok := update["eventsource"]; ok {
		t.Error("update must not send immutable eventsource")
	}
	for _, payload := range []map[string]interface{}{create, update} {
		if ops, ok := payload["operations"].([]interface{}); !ok || ops == nil {
			t.Errorf("operations must be an array, got %#v", payload["operations"])
		}
		filter, _ := payload["filter"].(map[string]interface{})
		if conds, ok := filter["conditions"].([]interface{}); !ok || conds == nil {
			t.Errorf("filter.conditions must be an array, got %#v", filter["conditions"])
		}
	}
}

func TestUpdateHost_NoInterfacesAndTemplatesClear(t *testing.T) {
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		return map[string][]string{"hostids": {"1"}}, nil
	})
	c := newTestClient(t, s, ClientConfig{APIToken: "t"})

	err := c.UpdateHost(context.Background(), "1", &HostSpec{Host: "h", GroupIDs: []string{"2"}}, []string{"10001"})
	if err != nil {
		t.Fatal(err)
	}
	var params map[string]interface{}
	_ = json.Unmarshal(s.calls("host.update")[0].Params, &params)
	if _, ok := params["interfaces"]; ok {
		t.Error("host.update must never send interfaces")
	}
	if tc, ok := params["templates_clear"].([]interface{}); !ok || len(tc) != 1 {
		t.Errorf("want templates_clear with 1 entry, got %#v", params["templates_clear"])
	}
	if _, ok := params["templates"].([]interface{}); !ok {
		t.Error("templates must be sent as an array (empty clears links)")
	}
}

func TestMutate_RejectsResultsWithoutIDs(t *testing.T) {
	for _, body := range []string{`true`, `false`, `{"hostids":[]}`, `{"groupids":["1"]}`, `{}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":` + body + `,"id":1}`))
		}))
		c, _ := NewZabbixClient(ClientConfig{URL: srv.URL, APIToken: "t"})
		if err := c.DeleteHost(context.Background(), "1"); err == nil {
			t.Errorf("result %s must not count as a successful host.delete", body)
		}
		srv.Close()
	}
}

func TestCall_FailedLoginMemoExpires(t *testing.T) {
	var logins atomic.Int32
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "user.login":
			switch logins.Add(1) {
			case 1:
				return "tok1", nil
			case 2:
				return nil, &JsonRpcError{Code: -32602, Message: "Invalid params.", Data: "Incorrect user name or password or account is temporarily blocked."}
			default:
				return "tok2", nil
			}
		case "hostgroup.get":
			if req.Auth != "Bearer tok2" {
				return nil, &JsonRpcError{Code: -32602, Message: "Invalid params.", Data: sessionTerminated}
			}
			return []HostGroup{{GroupID: "1", Name: "g"}}, nil
		}
		return nil, nil
	})
	c := newTestClient(t, s, passwordCfg)
	if err := c.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetHostGroup(context.Background(), "1"); err == nil {
		t.Fatal("first re-login must fail")
	}
	// Within the memo window the same failure is returned without a new attempt.
	if _, err := c.GetHostGroup(context.Background(), "1"); err == nil || logins.Load() != 2 {
		t.Fatalf("memoised failure expected, logins=%d err=%v", logins.Load(), err)
	}
	// After the memo expires a fresh attempt succeeds.
	c.mu.Lock()
	c.failed.at = time.Now().Add(-time.Minute)
	c.mu.Unlock()
	if _, err := c.GetHostGroup(context.Background(), "1"); err != nil {
		t.Fatalf("after memo expiry the login must be retried and succeed: %v", err)
	}
	if logins.Load() != 3 {
		t.Fatalf("want exactly one fresh login after expiry, got %d", logins.Load())
	}
}

func TestCall_MutationRetriedExactlyOnceAfterRelogin(t *testing.T) {
	// A mutation rejected with an expired session is safe to retry (Zabbix
	// rejects it before executing); it must be sent exactly twice in total.
	var logins, deletes atomic.Int32
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "user.login":
			if logins.Add(1) == 1 {
				return "tok1", nil
			}
			return "tok2", nil
		case "hostgroup.delete":
			if deletes.Add(1) == 1 {
				return nil, &JsonRpcError{Code: -32602, Message: "Invalid params.", Data: sessionTerminated}
			}
			if req.Auth != "Bearer tok2" {
				return nil, &JsonRpcError{Code: -32602, Message: "Invalid params.", Data: sessionTerminated}
			}
			return map[string][]string{"groupids": {"1"}}, nil
		}
		return nil, nil
	})
	c := newTestClient(t, s, passwordCfg)
	if err := c.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteHostGroup(context.Background(), "1"); err != nil {
		t.Fatalf("delete after re-login must succeed: %v", err)
	}
	if deletes.Load() != 2 || logins.Load() != 2 {
		t.Fatalf("want exactly 2 delete attempts and 2 logins, got %d/%d", deletes.Load(), logins.Load())
	}
}

func TestIsObjectMissing(t *testing.T) {
	err := &JsonRpcError{Code: -32500, Message: "Application error.", Data: objectMissing}
	if !IsObjectMissing(err) {
		t.Error("want true for missing object error")
	}
	if IsObjectMissing(errors.New("other")) {
		t.Error("want false for other errors")
	}
}
