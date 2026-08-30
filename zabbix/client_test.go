package zabbix

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gateway down", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	c, _ := NewZabbixClient(ClientConfig{URL: srv.URL, APIToken: "tok"})

	_, err := c.GetHost(context.Background(), "1")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("want transport error, got %v", err)
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
	var logins atomic.Int32
	s := newRPCServer(t, func(req rpcRequest) (interface{}, *JsonRpcError) {
		switch req.Method {
		case "user.login":
			n := logins.Add(1)
			return map[int32]string{1: "tok1", 2: "tok2"}[n], nil
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

	// 5 parallel calls hit an expired session: exactly one re-login must happen.
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
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
	if _, err := c.GetVersion(context.Background()); err != nil {
		t.Errorf("request with custom CA failed: %v", err)
	}

	untrusted, _ := NewZabbixClient(ClientConfig{URL: srv.URL, APIToken: "t"})
	if _, err := untrusted.GetVersion(context.Background()); err == nil {
		t.Error("self-signed certificate must be rejected without ca_cert_file")
	}
}

func TestMediaTypeParams_TypeAware(t *testing.T) {
	wh := mediaTypeParams(&MediaType{Type: "4", Script: "return 1;", Timeout: "30s"})
	if p, ok := wh["parameters"].([]MediaTypeParam); !ok || p == nil {
		t.Errorf("webhook without parameters must send an empty array, got %#v", wh["parameters"])
	}
	if _, ok := wh["smtp_server"]; ok {
		t.Error("webhook must not send smtp fields")
	}

	email := mediaTypeParams(&MediaType{Type: "0", SMTPServer: "mail", SMTPAuthentication: "0", Username: "u", Passwd: "p"})
	if _, ok := email["passwd"]; ok {
		t.Error("passwd must not be sent when smtp_authentication is 0")
	}
	if _, ok := email["script"]; ok {
		t.Error("email must not send webhook fields")
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
	if ops, ok := update["operations"].([]interface{}); !ok || ops == nil {
		t.Errorf("operations must be an array, got %#v", update["operations"])
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

func TestIsObjectMissing(t *testing.T) {
	err := &JsonRpcError{Code: -32500, Message: "Application error.", Data: objectMissing}
	if !IsObjectMissing(err) {
		t.Error("want true for missing object error")
	}
	if IsObjectMissing(errors.New("other")) {
		t.Error("want false for other errors")
	}
}
