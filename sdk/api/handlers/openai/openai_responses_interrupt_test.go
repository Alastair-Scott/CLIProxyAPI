package openai

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// The upstream cannot finish until it receives the interrupt. The next create
// must reuse that connection without treating the interrupt as queued steering.
func TestResponsesInterruptInFlightWebSocket(t *testing.T) {
	for _, boundary := range []string{"response.incomplete", "response.completed"} {
		t.Run(boundary, func(t *testing.T) {
			var connections atomic.Int32
			interrupt := []byte(`{"type":"response.interrupt","response_id":"r1","event_id":"interrupt-1","extension":{"preserve":true}}`)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connections.Add(1)
				c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = c.Close() }()
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				read := func() []byte {
					_, p, err := c.ReadMessage()
					if err != nil {
						t.Errorf("upstream read: %v", err)
					}
					return p
				}
				write := func(p []byte) {
					if err := c.WriteMessage(websocket.TextMessage, p); err != nil {
						t.Errorf("upstream write: %v", err)
					}
				}
				if p := read(); gjson.GetBytes(p, "type").String() != "response.create" {
					t.Errorf("initial create: %s", p)
					return
				}
				write([]byte(`{"type":"response.created","response":{"id":"r1","output":[]}}`))
				if p := read(); !bytes.Equal(p, interrupt) {
					t.Errorf("interrupt changed or blocked: %s", p)
					return
				}
				write([]byte(fmt.Sprintf(`{"type":%q,"response":{"id":"r1","output":[],"incomplete_details":{"reason":"interrupted"}}}`, boundary)))
				if p := read(); gjson.GetBytes(p, "type").String() != "response.create" {
					t.Errorf("follow-up create: %s", p)
					return
				}
				write([]byte(`{"type":"response.created","response":{"id":"r2","output":[]}}`))
				write([]byte(`{"type":"response.completed","response":{"id":"r2","output":[]}}`))
				// Wait for client cleanup rather than closing a healthy upstream socket.
				_, _, _ = c.ReadMessage()
			}))
			defer upstream.Close()
			cfg := &config.Config{}
			cfg.Codex.ResponseSteering = true
			cfg.CodexResponseSteering = true
			manager := coreauth.NewManager(nil, nil, nil)
			manager.SetConfig(cfg)
			manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
			authID, model := "interrupt-"+boundary, "interrupt-model-"+boundary
			if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: authID, Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": "test-key", "base_url": upstream.URL, "websockets": "true"}}); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
			defer registry.GetGlobalRegistry().UnregisterClient(authID)
			handler := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
			router := gin.New()
			router.GET("/v1/responses", handler.ResponsesWebsocket)
			downstream := httptest.NewServer(router)
			defer downstream.Close()
			c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			sendCreate := func() {
				if err := c.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.create","model":%q,"input":[]}`, model))); err != nil {
					t.Fatal(err)
				}
			}
			receive := func(kind, id string) {
				_, p, err := c.ReadMessage()
				if err != nil {
					t.Fatal(err)
				}
				if gjson.GetBytes(p, "type").String() != kind || gjson.GetBytes(p, "response.id").String() != id {
					t.Fatalf("want %s for %s, got %s", kind, id, p)
				}
			}
			sendCreate()
			receive("response.created", "r1")
			if err := c.WriteMessage(websocket.TextMessage, interrupt); err != nil {
				t.Fatal(err)
			}
			receive(boundary, "r1")
			sendCreate()
			receive("response.created", "r2")
			receive("response.completed", "r2")
			if got := connections.Load(); got != 1 {
				t.Fatalf("upstream connections=%d, want 1", got)
			}
		})
	}
}
