package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"

	"github.com/jaiakash/k8s-ai-agent/internal/policy"
)

// These tests drive the server over real JSON-RPC messages rather than calling
// handlers directly, so they cover the wire contract an MCP host actually
// sees: schemas, annotations, structured content and error shape.

func rpc(t *testing.T, s *server.MCPServer, ctx context.Context, request string) map[string]any {
	t.Helper()

	raw := s.HandleMessage(ctx, json.RawMessage(request))
	if raw == nil {
		return nil // a notification has no response
	}

	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return out
}

// handshake initializes a session and returns a context carrying it, which is
// what a real host holds for the rest of the connection.
func handshake(t *testing.T, s *server.MCPServer) context.Context {
	t.Helper()

	ctx := context.Background()
	resp := rpc(t, s, ctx, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
		"protocolVersion":"2025-06-18","capabilities":{},
		"clientInfo":{"name":"test-host","version":"1.0"}}}`)

	if errObj, ok := resp["error"]; ok {
		t.Fatalf("initialize failed: %v", errObj)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize returned no result: %v", resp)
	}
	if _, ok := result["serverInfo"].(map[string]any); !ok {
		t.Errorf("initialize must return serverInfo, got %v", result)
	}
	return ctx
}

func TestInitializeAdvertisesCapabilities(t *testing.T) {
	_, s := newRegistry(t, policy.Default(), true)
	ctx := context.Background()

	resp := rpc(t, s, ctx, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
		"protocolVersion":"2025-06-18","capabilities":{},
		"clientInfo":{"name":"test-host","version":"1.0"}}}`)

	result := resp["result"].(map[string]any)
	caps, ok := result["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("no capabilities in initialize result: %v", result)
	}
	for _, capability := range []string{"tools", "resources", "prompts"} {
		if _, ok := caps[capability]; !ok {
			t.Errorf("server should advertise the %q capability, got %v", capability, caps)
		}
	}
}

func TestToolsListOverTheWire(t *testing.T) {
	cfg := policy.Default()
	cfg.AllowWrites = true
	_, s := newRegistry(t, cfg, true)
	ctx := handshake(t, s)

	resp := rpc(t, s, ctx, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	if errObj, ok := resp["error"]; ok {
		t.Fatalf("tools/list failed: %v", errObj)
	}

	tools := resp["result"].(map[string]any)["tools"].([]any)
	if len(tools) != len(readToolNames)+len(writeToolNames) {
		t.Errorf("tools/list returned %d tools, want %d", len(tools), len(readToolNames)+len(writeToolNames))
	}

	byName := map[string]map[string]any{}
	for _, entry := range tools {
		tool := entry.(map[string]any)
		byName[tool["name"].(string)] = tool
	}

	// Every tool must reach the host with a usable input schema.
	for name, tool := range byName {
		if _, ok := tool["inputSchema"].(map[string]any); !ok {
			t.Errorf("tool %q has no inputSchema on the wire", name)
		}
	}

	// Annotations must survive serialization: they are what a host uses to
	// decide whether to ask a human before running a tool.
	deletePod, ok := byName["delete_pod"]
	if !ok {
		t.Fatal("delete_pod missing from tools/list")
	}
	annotations, ok := deletePod["annotations"].(map[string]any)
	if !ok {
		t.Fatalf("delete_pod has no annotations on the wire: %v", deletePod)
	}
	if annotations["destructiveHint"] != true {
		t.Errorf("delete_pod destructiveHint = %v, want true", annotations["destructiveHint"])
	}
	if annotations["readOnlyHint"] != false {
		t.Errorf("delete_pod readOnlyHint = %v, want false", annotations["readOnlyHint"])
	}

	listPods, ok := byName["list_pods"]
	if !ok {
		t.Fatal("list_pods missing from tools/list")
	}
	if listPods["annotations"].(map[string]any)["readOnlyHint"] != true {
		t.Error("list_pods must reach the host annotated readOnlyHint=true")
	}
}

func TestToolsCallOverTheWireReturnsStructuredContent(t *testing.T) {
	_, s := newRegistry(t, policy.Default(), true,
		testPod("web", "default", true))
	ctx := handshake(t, s)

	resp := rpc(t, s, ctx, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{
		"name":"list_pods","arguments":{"namespace":"default"}}}`)
	if errObj, ok := resp["error"]; ok {
		t.Fatalf("tools/call failed: %v", errObj)
	}

	result := resp["result"].(map[string]any)
	if result["isError"] == true {
		t.Fatalf("tool reported an error: %v", result["content"])
	}

	structuredContent, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("no structuredContent on the wire: %v", result)
	}
	if structuredContent["unhealthy"].(float64) != 1 {
		t.Errorf("unhealthy = %v, want 1", structuredContent["unhealthy"])
	}

	// A tool returning structured content must also return text, so hosts
	// that do not read structuredContent still show something useful.
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("structured results must still carry text content: %v", result)
	}
}

func TestToolErrorsAreInBandNotProtocolErrors(t *testing.T) {
	// An RBAC denial must come back as isError on a successful JSON-RPC
	// response, so the model can read it and adapt. A protocol-level error
	// would be invisible to the model.
	_, s := newRegistry(t, policy.Default(), false, testPod("web", "default", false))
	ctx := handshake(t, s)

	resp := rpc(t, s, ctx, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{
		"name":"list_pods","arguments":{"namespace":"default"}}}`)

	if _, ok := resp["error"]; ok {
		t.Fatalf("a tool denial must not be a protocol error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Errorf("denial should set isError=true, got %v", result)
	}
}

func TestUnknownToolIsRejected(t *testing.T) {
	_, s := newRegistry(t, policy.Default(), true)
	ctx := handshake(t, s)

	resp := rpc(t, s, ctx, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{
		"name":"rm_minus_rf","arguments":{}}}`)

	_, hasErr := resp["error"]
	result, hasResult := resp["result"].(map[string]any)
	if !hasErr && !(hasResult && result["isError"] == true) {
		t.Errorf("an unknown tool must be rejected, got %v", resp)
	}
}

func TestWriteToolInvisibleOverTheWireWhenReadOnly(t *testing.T) {
	_, s := newRegistry(t, policy.Default(), true, testPod("web", "default", false))
	ctx := handshake(t, s)

	// Not listed...
	resp := rpc(t, s, ctx, `{"jsonrpc":"2.0","id":6,"method":"tools/list","params":{}}`)
	for _, entry := range resp["result"].(map[string]any)["tools"].([]any) {
		if entry.(map[string]any)["name"] == "delete_pod" {
			t.Fatal("delete_pod must not be listed under a read-only policy")
		}
	}

	// ...and not callable either, even if a host asks for it by name.
	resp = rpc(t, s, ctx, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{
		"name":"delete_pod","arguments":{"namespace":"default","name":"web","dry_run":false}}}`)

	_, hasErr := resp["error"]
	result, hasResult := resp["result"].(map[string]any)
	if !hasErr && !(hasResult && result["isError"] == true) {
		t.Errorf("delete_pod must be uncallable under a read-only policy, got %v", resp)
	}
}

func TestResourcesReadOverTheWire(t *testing.T) {
	_, s := newRegistry(t, policy.Default(), true, testPod("web", "default", true))
	ctx := handshake(t, s)

	resp := rpc(t, s, ctx, `{"jsonrpc":"2.0","id":8,"method":"resources/read","params":{
		"uri":"k8s://policy"}}`)
	if errObj, ok := resp["error"]; ok {
		t.Fatalf("resources/read failed: %v", errObj)
	}

	contents := resp["result"].(map[string]any)["contents"].([]any)
	if len(contents) == 0 {
		t.Fatal("k8s://policy returned no contents")
	}

	text := contents[0].(map[string]any)["text"].(string)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("k8s://policy is not valid JSON: %v", err)
	}
	if parsed["allowWrites"] != false {
		t.Errorf("policy resource should report allowWrites=false, got %v", parsed["allowWrites"])
	}
}

func TestPromptsGetOverTheWire(t *testing.T) {
	_, s := newRegistry(t, policy.Default(), true)
	ctx := handshake(t, s)

	resp := rpc(t, s, ctx, `{"jsonrpc":"2.0","id":9,"method":"prompts/get","params":{
		"name":"diagnose_pod","arguments":{"namespace":"default","name":"web"}}}`)
	if errObj, ok := resp["error"]; ok {
		t.Fatalf("prompts/get failed: %v", errObj)
	}

	result := resp["result"].(map[string]any)
	messages := result["messages"].([]any)
	if len(messages) == 0 {
		t.Fatal("diagnose_pod returned no messages")
	}

	text := messages[0].(map[string]any)["content"].(map[string]any)["text"].(string)
	// The playbook must be specific to the pod it was asked about, and must
	// name the tools it expects the model to call.
	if !strings.Contains(text, "web") || !strings.Contains(text, "default") {
		t.Errorf("prompt should be specialized to the requested pod, got:\n%s", text)
	}
	for _, tool := range []string{"get_pod", "get_pod_logs"} {
		if !strings.Contains(text, tool) {
			t.Errorf("prompt should reference the %s tool, got:\n%s", tool, text)
		}
	}
}

func TestPromptRejectsMissingArguments(t *testing.T) {
	_, s := newRegistry(t, policy.Default(), true)
	ctx := handshake(t, s)

	resp := rpc(t, s, ctx, `{"jsonrpc":"2.0","id":10,"method":"prompts/get","params":{
		"name":"diagnose_pod","arguments":{"namespace":"default"}}}`)

	if _, ok := resp["error"]; !ok {
		t.Errorf("diagnose_pod without a pod name should fail, got %v", resp)
	}
}

func TestServerInstructionsReachTheHost(t *testing.T) {
	// Instructions are how a host learns the dry-run workflow and the meaning
	// of the policy resource, so they must actually arrive at initialize.
	const want = "call once with dry_run, then commit"

	s := server.NewMCPServer("kai", "test", server.WithInstructions(want))

	resp := rpc(t, s, context.Background(), `{"jsonrpc":"2.0","id":11,"method":"initialize","params":{
		"protocolVersion":"2025-06-18","capabilities":{},
		"clientInfo":{"name":"test-host","version":"1.0"}}}`)

	result := resp["result"].(map[string]any)
	got, ok := result["instructions"].(string)
	if !ok {
		t.Fatalf("initialize returned no instructions: %v", result)
	}
	if !strings.Contains(got, want) {
		t.Errorf("instructions = %q, want them to contain %q", got, want)
	}
}
