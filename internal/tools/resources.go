package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jaiakash/k8s-ai-agent/internal/k8s"
	"github.com/jaiakash/k8s-ai-agent/internal/policy"
)

// MCP resources are context a host can attach without the model deciding to
// call anything. Cluster identity and overall health are exactly that: facts
// worth having in context before the first question, not results of a tool
// call the model has to think to make.
func (r *Registry) registerResources(s *server.MCPServer) {
	s.AddResource(mcp.NewResource(
		"k8s://cluster/info",
		"Cluster info",
		mcp.WithResourceDescription("Server version, node and namespace counts, and metrics API availability for the cluster KAI is connected to."),
		mcp.WithMIMEType("application/json"),
	), r.clusterInfoResource)

	s.AddResource(mcp.NewResource(
		"k8s://cluster/health",
		"Cluster health",
		mcp.WithResourceDescription("Every unhealthy pod in the cluster with the reason it is unhealthy, plus any node that is not ready. Read this for an at-a-glance triage picture."),
		mcp.WithMIMEType("application/json"),
	), r.clusterHealthResource)

	s.AddResource(mcp.NewResource(
		"k8s://namespaces",
		"Namespaces",
		mcp.WithResourceDescription("Namespaces visible to this agent."),
		mcp.WithMIMEType("application/json"),
	), r.namespacesResource)

	s.AddResource(mcp.NewResource(
		"k8s://policy",
		"Active policy",
		mcp.WithResourceDescription("The guardrails this KAI instance enforces: whether writes are permitted, which namespaces are protected or hidden, and whether writes are pinned to dry run. Read this to know what you are allowed to do."),
		mcp.WithMIMEType("application/json"),
	), r.policyResource)
}

// jsonResource marshals v as a JSON resource body at uri.
func jsonResource(uri string, v any) ([]mcp.ResourceContents, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", uri, err)
	}
	return []mcp.ResourceContents{mcp.TextResourceContents{
		URI:      uri,
		MIMEType: "application/json",
		Text:     string(data),
	}}, nil
}

func (r *Registry) clusterInfoResource(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	info, err := r.Client.ClusterInfo(ctx)
	if err != nil {
		return nil, err
	}
	return jsonResource(req.Params.URI, info)
}

func (r *Registry) namespacesResource(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	if err := r.check(ctx, policy.Action{Verb: policy.VerbList, Resource: "namespaces"}); err != nil {
		return nil, err
	}
	list, err := r.Client.ListNamespaces(ctx)
	if err != nil {
		return nil, err
	}
	return jsonResource(req.Params.URI, map[string]any{"namespaces": list, "count": len(list)})
}

// ClusterHealth is the body of k8s://cluster/health.
type ClusterHealth struct {
	UnhealthyPods []k8s.PodSummary  `json:"unhealthyPods"`
	NotReadyNodes []k8s.NodeSummary `json:"notReadyNodes"`
	TotalPods     int               `json:"totalPods"`
	TotalNodes    int               `json:"totalNodes"`
	Healthy       bool              `json:"healthy"`
	Summary       string            `json:"summary"`
}

func (r *Registry) clusterHealthResource(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	if err := r.check(ctx, policy.Action{Verb: policy.VerbList, Resource: "pods"}); err != nil {
		return nil, err
	}

	pods, err := r.Client.ListPods(ctx, "", "", "", 0)
	if err != nil {
		return nil, err
	}

	health := &ClusterHealth{TotalPods: pods.Count, UnhealthyPods: []k8s.PodSummary{}}
	for _, p := range pods.Pods {
		if p.Issue != "" {
			health.UnhealthyPods = append(health.UnhealthyPods, p)
		}
	}

	// Nodes are best-effort: a namespace-scoped caller can read pods without
	// holding cluster-wide node access, and a partial picture still helps.
	health.NotReadyNodes = []k8s.NodeSummary{}
	if nodes, err := r.Client.ListNodes(ctx); err == nil {
		health.TotalNodes = len(nodes)
		for _, n := range nodes {
			if n.Status != "Ready" || !n.Schedulable {
				health.NotReadyNodes = append(health.NotReadyNodes, n)
			}
		}
	}

	health.Healthy = len(health.UnhealthyPods) == 0 && len(health.NotReadyNodes) == 0
	health.Summary = fmt.Sprintf("%d/%d pods unhealthy, %d/%d nodes not ready or cordoned",
		len(health.UnhealthyPods), health.TotalPods, len(health.NotReadyNodes), health.TotalNodes)

	return jsonResource(req.Params.URI, health)
}

func (r *Registry) policyResource(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	cfg := r.Policy.Config()
	return jsonResource(req.Params.URI, map[string]any{
		"allowWrites":         cfg.AllowWrites,
		"dryRunOnly":          cfg.DryRunOnly,
		"protectedNamespaces": cfg.ProtectedNamespaces,
		"allowedNamespaces":   cfg.AllowedNamespaces,
		"deniedNamespaces":    cfg.DeniedNamespaces,
		"maxLogLines":         cfg.MaxLogLines,
		"enforceRBAC":         cfg.EnforceRBAC,
		"registeredTools":     r.ToolNames(),
	})
}

// check is the resource-side equivalent of guard: resources bypass the tool
// path, so they must run the same policy and RBAC checks or they would be a
// way around them.
func (r *Registry) check(ctx context.Context, a policy.Action) error {
	if err := r.Policy.Check(a); err != nil {
		return err
	}
	if !r.Policy.EnforceRBAC() {
		return nil
	}
	return r.Client.Can(ctx, a)
}
