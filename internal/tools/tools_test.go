package tools

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/jaiakash/k8s-ai-agent/internal/k8s"
	"github.com/jaiakash/k8s-ai-agent/internal/policy"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newRegistry builds a Registry backed by a fake cluster. rbacAllowed decides
// how the fake API server answers SelfSubjectAccessReview.
func newRegistry(t *testing.T, cfg policy.Config, rbacAllowed bool, objs ...runtime.Object) (*Registry, *server.MCPServer) {
	t.Helper()

	cs := fake.NewClientset(objs...)
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authzv1.SelfSubjectAccessReview{
			Status: authzv1.SubjectAccessReviewStatus{
				Allowed: rbacAllowed,
				Reason:  "fake authorizer",
			},
		}, nil
	})

	r := New(&k8s.Client{Typed: cs, ContextName: "test"}, policy.New(cfg), quietLogger())
	s := server.NewMCPServer("kai", "test")
	r.Register(s)
	return r, s
}

func testPod(name, ns string, issue bool) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: true}},
		},
	}
	if issue {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  "app",
			Ready: false,
			State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
			},
		}}
	}
	return p
}

func call(t *testing.T, s *server.MCPServer, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()

	tool := s.GetTool(name)
	if tool == nil {
		t.Fatalf("tool %q is not registered", name)
	}

	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args

	result, err := tool.Handler(context.Background(), req)
	if err != nil {
		t.Fatalf("%s returned a protocol error: %v", name, err)
	}
	return result
}

func resultText(result *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range result.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func TestReadOnlyPolicyRegistersNoWriteTools(t *testing.T) {
	_, s := newRegistry(t, policy.Default(), true)

	registered := s.ListTools()
	for _, name := range writeToolNames {
		if _, found := registered[name]; found {
			t.Errorf("write tool %q must not be registered under a read-only policy", name)
		}
	}
	// The read tools must still all be there.
	for _, name := range readToolNames {
		if _, found := registered[name]; !found {
			t.Errorf("read tool %q should be registered", name)
		}
	}
}

func TestAllowWritesRegistersWriteTools(t *testing.T) {
	cfg := policy.Default()
	cfg.AllowWrites = true
	_, s := newRegistry(t, cfg, true)

	registered := s.ListTools()
	for _, name := range writeToolNames {
		if _, found := registered[name]; !found {
			t.Errorf("write tool %q should be registered when allowWrites is set", name)
		}
	}
}

func TestToolAnnotationsMatchBehaviour(t *testing.T) {
	cfg := policy.Default()
	cfg.AllowWrites = true
	_, s := newRegistry(t, cfg, true)

	// Hosts use these hints to decide whether to prompt a human, so an
	// incorrect annotation is a safety bug, not a documentation nit.
	for _, name := range readToolNames {
		tool := s.GetTool(name)
		if tool.Tool.Annotations.ReadOnlyHint == nil || !*tool.Tool.Annotations.ReadOnlyHint {
			t.Errorf("%s must be annotated readOnlyHint=true", name)
		}
		if tool.Tool.Annotations.DestructiveHint != nil && *tool.Tool.Annotations.DestructiveHint {
			t.Errorf("%s must not be annotated destructive", name)
		}
	}

	for _, name := range []string{"delete_pod", "scale_deployment", "restart_deployment", "apply_manifest"} {
		tool := s.GetTool(name)
		if tool.Tool.Annotations.ReadOnlyHint == nil || *tool.Tool.Annotations.ReadOnlyHint {
			t.Errorf("%s must be annotated readOnlyHint=false", name)
		}
		if tool.Tool.Annotations.DestructiveHint == nil || !*tool.Tool.Annotations.DestructiveHint {
			t.Errorf("%s must be annotated destructiveHint=true", name)
		}
	}
}

func TestWriteToolsDefaultToDryRun(t *testing.T) {
	cfg := policy.Default()
	cfg.AllowWrites = true
	_, s := newRegistry(t, cfg, true)

	for _, name := range writeToolNames {
		tool := s.GetTool(name)
		props := tool.Tool.InputSchema.Properties
		raw, ok := props["dry_run"]
		if !ok {
			t.Errorf("%s must expose a dry_run parameter", name)
			continue
		}
		schema, ok := raw.(map[string]any)
		if !ok {
			t.Errorf("%s dry_run schema has unexpected type %T", name, raw)
			continue
		}
		if def, ok := schema["default"].(bool); !ok || !def {
			t.Errorf("%s dry_run must default to true, got %v", name, schema["default"])
		}
	}
}

func TestListPodsReturnsStructuredAndTextContent(t *testing.T) {
	_, s := newRegistry(t, policy.Default(), true,
		testPod("healthy", "default", false),
		testPod("broken", "default", true))

	result := call(t, s, "list_pods", map[string]any{"namespace": "default"})
	if result.IsError {
		t.Fatalf("list_pods failed: %s", resultText(result))
	}

	list, ok := result.StructuredContent.(*k8s.PodList)
	if !ok {
		t.Fatalf("StructuredContent is %T, want *k8s.PodList", result.StructuredContent)
	}
	if list.Unhealthy != 1 {
		t.Errorf("Unhealthy = %d, want 1", list.Unhealthy)
	}

	// The text fallback must carry the same facts for hosts that ignore
	// structured content.
	text := resultText(result)
	if !strings.Contains(text, "CrashLoopBackOff") {
		t.Errorf("text fallback should name the issue, got:\n%s", text)
	}
	if !strings.Contains(text, "broken") {
		t.Errorf("text fallback should list the pod, got:\n%s", text)
	}
}

func TestPolicyDenialIsReportedAsToolError(t *testing.T) {
	cfg := policy.Default()
	cfg.DeniedNamespaces = []string{"secret-ns"}
	_, s := newRegistry(t, cfg, true, testPod("web", "secret-ns", false))

	result := call(t, s, "list_pods", map[string]any{"namespace": "secret-ns"})
	if !result.IsError {
		t.Fatal("a denied namespace must produce a tool error")
	}
	if !strings.Contains(resultText(result), "deniedNamespaces") {
		t.Errorf("the denial should explain the rule, got: %s", resultText(result))
	}
}

func TestRBACDenialIsReportedBeforeTheCall(t *testing.T) {
	_, s := newRegistry(t, policy.Default(), false, testPod("web", "default", false))

	result := call(t, s, "list_pods", map[string]any{"namespace": "default"})
	if !result.IsError {
		t.Fatal("an RBAC denial must produce a tool error")
	}
	text := resultText(result)
	if !strings.Contains(text, "RBAC") {
		t.Errorf("the denial should say it is an RBAC problem, got: %s", text)
	}
	// The point of pre-flight is that the user learns this before anything ran.
	if !strings.Contains(text, "default") {
		t.Errorf("the denial should name the namespace attempted, got: %s", text)
	}
}

func TestRBACCheckSkippedWhenDisabled(t *testing.T) {
	cfg := policy.Default()
	cfg.EnforceRBAC = false
	// The fake authorizer denies, but with enforcement off it is never asked.
	_, s := newRegistry(t, cfg, false, testPod("web", "default", false))

	result := call(t, s, "list_pods", map[string]any{"namespace": "default"})
	if result.IsError {
		t.Fatalf("with enforceRBAC=false the call should proceed, got: %s", resultText(result))
	}
}

func TestWriteToolRefusedInProtectedNamespace(t *testing.T) {
	cfg := policy.Default()
	cfg.AllowWrites = true
	_, s := newRegistry(t, cfg, true, testPod("coredns", "kube-system", false))

	result := call(t, s, "delete_pod", map[string]any{
		"namespace": "kube-system", "name": "coredns", "dry_run": false,
	})
	if !result.IsError {
		t.Fatal("writes to a protected namespace must be refused")
	}
	if !strings.Contains(resultText(result), "protected") {
		t.Errorf("refusal should cite the protection, got: %s", resultText(result))
	}
}

func TestDryRunOnlyPolicyOverridesExplicitCommit(t *testing.T) {
	cfg := policy.Default()
	cfg.AllowWrites = true
	cfg.DryRunOnly = true
	_, s := newRegistry(t, cfg, true, testPod("web", "default", false))

	// The caller explicitly asks to commit; policy must win and say so.
	result := call(t, s, "delete_pod", map[string]any{
		"namespace": "default", "name": "web", "dry_run": false,
	})
	if result.IsError {
		t.Fatalf("delete_pod failed: %s", resultText(result))
	}

	write, ok := result.StructuredContent.(*k8s.WriteResult)
	if !ok {
		t.Fatalf("StructuredContent is %T, want *k8s.WriteResult", result.StructuredContent)
	}
	if !write.DryRun {
		t.Error("dryRunOnly policy must force DryRun=true")
	}
	if !strings.Contains(write.Message, "dryRunOnly") {
		t.Errorf("the result must tell the caller the request was overridden, got: %q", write.Message)
	}
}

func TestGetPodRequiresItsArguments(t *testing.T) {
	_, s := newRegistry(t, policy.Default(), true)

	result := call(t, s, "get_pod", map[string]any{"namespace": "default"})
	if !result.IsError {
		t.Fatal("a missing required argument must be a tool error")
	}
}

func TestPromptsAndResourcesAreRegistered(t *testing.T) {
	_, s := newRegistry(t, policy.Default(), true)

	prompts := s.ListPrompts()
	for _, name := range []string{"diagnose_pod", "triage_namespace", "capacity_review"} {
		if _, ok := prompts[name]; !ok {
			t.Errorf("prompt %q should be registered", name)
		}
	}

	resources := s.ListResources()
	for _, uri := range []string{"k8s://cluster/info", "k8s://cluster/health", "k8s://namespaces", "k8s://policy"} {
		if _, ok := resources[uri]; !ok {
			t.Errorf("resource %q should be registered", uri)
		}
	}
}

func TestPolicyResourceReportsRegisteredTools(t *testing.T) {
	cfg := policy.Default()
	cfg.AllowWrites = true
	r, _ := newRegistry(t, cfg, true)

	names := r.ToolNames()
	if !slices.Contains(names, "list_pods") {
		t.Error("ToolNames should include read tools")
	}
	if !slices.Contains(names, "delete_pod") {
		t.Error("ToolNames should include write tools when writes are allowed")
	}
}

func TestToolDescriptionsExplainWhenToUseThem(t *testing.T) {
	cfg := policy.Default()
	cfg.AllowWrites = true
	_, s := newRegistry(t, cfg, true)

	// A model picks tools from descriptions alone, so a bare restatement of
	// the name is not enough for any of them.
	for name, tool := range s.ListTools() {
		if len(tool.Tool.Description) < 40 {
			t.Errorf("tool %q has a description too short to guide a model: %q", name, tool.Tool.Description)
		}
	}
}
