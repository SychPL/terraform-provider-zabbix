package zabbix

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

type mockRPCServer struct {
	*httptest.Server
	requests []string
	handler  func(method string, params json.RawMessage) (interface{}, *JsonRpcError)
	mu       sync.Mutex
}

func newMockRPCServer(t *testing.T, handler func(method string, params json.RawMessage) (interface{}, *JsonRpcError)) *mockRPCServer {
	t.Helper()
	s := &mockRPCServer{handler: handler}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Jsonrpc string          `json:"jsonrpc"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
			ID      int             `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("failed to decode jsonrpc: %s", err)
		}

		s.mu.Lock()
		s.requests = append(s.requests, payload.Method)
		s.mu.Unlock()

		result, rpcErr := s.handler(payload.Method, payload.Params)
		resp := map[string]interface{}{"jsonrpc": "2.0", "id": payload.ID}
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

func (s *mockRPCServer) getRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.requests))
	copy(out, s.requests)
	return out
}

func TestIsObjectMissing(t *testing.T) {
	errMatch := &JsonRpcError{
		Code:    -32602,
		Message: "Invalid params.",
		Data:    "No permissions to referred object or it does not exist!",
	}
	if !IsObjectMissing(errMatch) {
		t.Errorf("expected IsObjectMissing true for matching data")
	}

	errMismatch := &JsonRpcError{
		Code:    -32602,
		Message: "Invalid params.",
		Data:    "Some other error message.",
	}
	if IsObjectMissing(errMismatch) {
		t.Errorf("expected IsObjectMissing false for mismatching data")
	}

	if IsObjectMissing(errors.New("generic error")) {
		t.Errorf("expected IsObjectMissing false for generic errors")
	}
}

func TestGetHostGroup_NotFoundVsError(t *testing.T) {
	var fail atomic.Bool
	s := newMockRPCServer(t, func(method string, params json.RawMessage) (interface{}, *JsonRpcError) {
		switch method {
		case "user.login":
			return "auth_token_xyz", nil
		case "hostgroup.get":
			if fail.Load() {
				return nil, &JsonRpcError{Code: -32500, Message: "Database down."}
			}
			return []HostGroup{}, nil
		}
		return nil, nil
	})

	c, err := NewZabbixClient(s.URL, "admin", "zabbix", "", false, "")
	if err != nil {
		t.Fatalf("failed to create client: %s", err)
	}

	_, err = c.GetHostGroup(context.Background(), "1")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound for empty results, got %v", err)
	}

	fail.Store(true)
	_, err = c.GetHostGroup(context.Background(), "1")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("expected database error, got %v", err)
	}
}

func TestCall_ReloginOnceOnSessionExpiry(t *testing.T) {
	var requestCount int32
	s := newMockRPCServer(t, func(method string, params json.RawMessage) (interface{}, *JsonRpcError) {
		switch method {
		case "user.login":
			return "new_token", nil
		case "hostgroup.get":
			if atomic.AddInt32(&requestCount, 1) == 1 {
				return nil, &JsonRpcError{
					Code:    -32602,
					Message: "Session expired.",
				}
			}
			return []HostGroup{{GroupID: "1", Name: "group1"}}, nil
		}
		return nil, nil
	})

	c, err := NewZabbixClient(s.URL, "admin", "zabbix", "", false, "")
	if err != nil {
		t.Fatalf("failed to create client: %s", err)
	}
	c.SessionToken = "expired_token"

	// Trigger request - should fail once, re-login, and retry
	res, err := c.GetHostGroup(context.Background(), "1")
	if err != nil {
		t.Fatalf("expected successful call after login, got: %s", err)
	}

	if res.Name != "group1" {
		t.Errorf("expected group1, got %s", res.Name)
	}

	reqs := s.getRequests()
	// Should have called: hostgroup.get (expired), user.login, hostgroup.get (retry)
	var loginCount, getCount int
	for _, m := range reqs {
		if m == "user.login" {
			loginCount++
		} else if m == "hostgroup.get" {
			getCount++
		}
	}

	if loginCount != 1 {
		t.Errorf("expected 1 user.login call, got %d", loginCount)
	}
	if getCount != 2 {
		t.Errorf("expected 2 hostgroup.get calls, got %d", getCount)
	}
}
