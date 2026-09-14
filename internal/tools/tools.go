// Package tools registers KAI's Kubernetes capabilities on an MCP server.
//
// There is deliberately no LLM in this package, or anywhere else in this
// server. KAI exposes cluster capabilities; the MCP host (Claude Code, Goose,
// Cursor, an agentgateway-fronted service) brings its own model. That split is
// what makes the server usable from any host and testable without one.
package tools

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jaiakash/k8s-ai-agent/internal/k8s"
	"github.com/jaiakash/k8s-ai-agent/internal/policy"
)

// Registry binds a cluster client and a policy engine to an MCP server.
type Registry struct {
	Client *k8s.Client
	Policy *policy.Engine
	Log    *slog.Logger
}

// New returns a Registry.
func New(client *k8s.Client, engine *policy.Engine, log *slog.Logger) *Registry {
	if log == nil {
		log = slog.Default()
	}
	return &Registry{Client: client, Policy: engine, Log: log}
}

// Register adds every tool, resource and prompt this policy permits.
//
// Write tools are not registered at all under a read-only policy, rather than
// registered and then refused. A tool the model cannot see is a tool it cannot
// be talked into calling, and the tool list doubles as honest documentation of
// what this instance can do.
func (r *Registry) Register(s *server.MCPServer) {
	r.registerReadTools(s)
	if r.Policy.AllowWrites() {
		r.registerWriteTools(s)
	}
	r.registerResources(s)
	r.registerPrompts(s)
}

// ToolNames lists the tools that would be registered, for startup logging.
func (r *Registry) ToolNames() []string {
	names := append([]string{}, readToolNames...)
	if r.Policy.AllowWrites() {
		names = append(names, writeToolNames...)
	}
	return names
}

// guard runs local policy and then RBAC pre-flight for every action a tool is
// about to perform. A non-nil return is a tool error to send straight back.
//
// Both checks run, in this order, for every single tool call: policy is cheap
// and local, so it rejects out-of-scope requests without a network round trip,
// and RBAC pre-flight then reports a permissions problem *before* anything is
// attempted rather than as an opaque 403 afterwards.
func (r *Registry) guard(ctx context.Context, actions ...policy.Action) *mcp.CallToolResult {
	for _, a := range actions {
		if err := r.Policy.Check(a); err != nil {
			r.Log.Warn("policy denied tool call", "action", a.String(), "error", err)
			return mcp.NewToolResultError(err.Error())
		}
		if !r.Policy.EnforceRBAC() {
			continue
		}
		if err := r.Client.Can(ctx, a); err != nil {
			r.Log.Warn("rbac denied tool call", "action", a.String(), "error", err)
			return mcp.NewToolResultError(err.Error())
		}
	}
	return nil
}

// readOnly marks a tool that cannot change cluster state.
func readOnly() mcp.ToolOption {
	return mcp.WithToolAnnotation(mcp.ToolAnnotation{
		ReadOnlyHint:    ptr(true),
		DestructiveHint: ptr(false),
		IdempotentHint:  ptr(true),
		OpenWorldHint:   ptr(false),
	})
}

// mutating marks a tool that changes cluster state. destructive should be true
// when the change can disrupt a running workload, which is what MCP hosts use
// to decide whether to ask a human first.
func mutating(destructive, idempotent bool) mcp.ToolOption {
	return mcp.WithToolAnnotation(mcp.ToolAnnotation{
		ReadOnlyHint:    ptr(false),
		DestructiveHint: ptr(destructive),
		IdempotentHint:  ptr(idempotent),
		OpenWorldHint:   ptr(false),
	})
}

func ptr[T any](v T) *T { return &v }

// namespaceArg is the shared description for the optional namespace parameter,
// so every tool documents the all-namespaces behaviour identically.
const namespaceArg = "Namespace to query. Omit to query across all namespaces, " +
	"which requires cluster-wide read permission."

// errorf returns a tool error, keeping call sites to one line.
func errorf(format string, args ...any) *mcp.CallToolResult {
	return mcp.NewToolResultError(fmt.Sprintf(format, args...))
}

// resultErr converts a Go error into a tool error result. Errors here are
// expected outcomes (not found, forbidden), so they are reported to the model
// as tool errors it can reason about, not as protocol failures.
func resultErr(err error) *mcp.CallToolResult {
	return mcp.NewToolResultError(err.Error())
}

// structured returns both a machine-readable payload and a text fallback.
//
// Hosts that understand structured content get typed JSON; older ones still
// render something a human can read. Sending only one of the two would break
// one audience or the other.
func structured(data any, summary string) *mcp.CallToolResult {
	return mcp.NewToolResultStructured(data, summary)
}

// table renders rows as an aligned text block for the fallback summary.
func table(header []string, rows [][]string) string {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	var sb strings.Builder
	writeRow := func(cells []string) {
		for i, cell := range cells {
			if i > 0 {
				sb.WriteString("  ")
			}
			sb.WriteString(cell)
			if i < len(cells)-1 {
				sb.WriteString(strings.Repeat(" ", widths[i]-len(cell)))
			}
		}
		sb.WriteString("\n")
	}

	writeRow(header)
	for _, row := range rows {
		writeRow(row)
	}
	return sb.String()
}
