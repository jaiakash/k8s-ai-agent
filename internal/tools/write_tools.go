package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jaiakash/k8s-ai-agent/internal/k8s"
	"github.com/jaiakash/k8s-ai-agent/internal/policy"
)

var writeToolNames = []string{
	"scale_deployment", "restart_deployment", "delete_pod",
	"cordon_node", "uncordon_node", "apply_manifest",
}

// dryRunArg documents the shared dry_run parameter. Every write tool defaults
// it to true: the agent's first attempt at a change is always a rehearsal, and
// committing requires the caller to say so explicitly.
const dryRunArg = "Validate the change against the API server without applying it. " +
	"Defaults to true. Call once with the default to preview the outcome, then " +
	"again with dry_run=false to commit."

func (r *Registry) registerWriteTools(s *server.MCPServer) {
	s.AddTool(mcp.NewTool("scale_deployment",
		mcp.WithDescription("Change a deployment's replica count. Scaling to 0 stops the workload. Returns the previous and new counts."),
		mcp.WithToolTitle("Scale deployment"),
		mutating(true, true),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Namespace containing the deployment.")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Deployment name.")),
		mcp.WithNumber("replicas", mcp.Required(), mcp.Description("Desired replica count."), mcp.Min(0), mcp.Max(1000)),
		mcp.WithBoolean("dry_run", mcp.Description(dryRunArg), mcp.DefaultBool(true)),
		mcp.WithOutputSchema[k8s.WriteResult](),
	), r.scaleDeployment)

	s.AddTool(mcp.NewTool("restart_deployment",
		mcp.WithDescription("Trigger a rolling restart of a deployment, the equivalent of 'kubectl rollout restart'. Pods are replaced gradually under the deployment's rollout strategy; none are deleted directly."),
		mcp.WithToolTitle("Restart deployment"),
		mutating(true, false),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Namespace containing the deployment.")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Deployment name.")),
		mcp.WithBoolean("dry_run", mcp.Description(dryRunArg), mcp.DefaultBool(true)),
		mcp.WithOutputSchema[k8s.WriteResult](),
	), r.restartDeployment)

	s.AddTool(mcp.NewTool("delete_pod",
		mcp.WithDescription("Delete a single pod. A pod managed by a Deployment, StatefulSet or DaemonSet is recreated immediately, which makes this a way to restart one replica. Deleting an unmanaged pod destroys it permanently."),
		mcp.WithToolTitle("Delete pod"),
		mutating(true, false),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Namespace containing the pod.")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Pod name.")),
		mcp.WithBoolean("dry_run", mcp.Description(dryRunArg), mcp.DefaultBool(true)),
		mcp.WithOutputSchema[k8s.WriteResult](),
	), r.deletePod)

	s.AddTool(mcp.NewTool("cordon_node",
		mcp.WithDescription("Mark a node unschedulable so no new pods land on it. Pods already running on the node are NOT evicted and keep serving traffic."),
		mcp.WithToolTitle("Cordon node"),
		mutating(true, true),
		mcp.WithString("name", mcp.Required(), mcp.Description("Node name.")),
		mcp.WithBoolean("dry_run", mcp.Description(dryRunArg), mcp.DefaultBool(true)),
		mcp.WithOutputSchema[k8s.WriteResult](),
	), r.cordonNode)

	s.AddTool(mcp.NewTool("uncordon_node",
		mcp.WithDescription("Mark a node schedulable again so the scheduler may place new pods on it."),
		mcp.WithToolTitle("Uncordon node"),
		mutating(false, true),
		mcp.WithString("name", mcp.Required(), mcp.Description("Node name.")),
		mcp.WithBoolean("dry_run", mcp.Description(dryRunArg), mcp.DefaultBool(true)),
		mcp.WithOutputSchema[k8s.WriteResult](),
	), r.uncordonNode)

	s.AddTool(mcp.NewTool("apply_manifest",
		mcp.WithDescription("Server-side apply a YAML or JSON manifest, which may hold several documents separated by '---'. Under a dry run the API server runs admission, validation, quota checks and webhooks, then discards the result, so this is a true rehearsal of the change."),
		mcp.WithToolTitle("Apply manifest"),
		mutating(true, true),
		mcp.WithString("manifest", mcp.Required(), mcp.Description("The manifest to apply, as YAML or JSON.")),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Namespace for namespaced objects. Objects that name a different namespace are rejected.")),
		mcp.WithBoolean("dry_run", mcp.Description(dryRunArg), mcp.DefaultBool(true)),
		mcp.WithOutputSchema[k8s.ApplyResult](),
	), r.applyManifest)
}

// resolveDryRun applies the dry_run argument and the DryRunOnly policy.
//
// When policy pins the instance to dry-run, an explicit dry_run=false is
// overridden rather than honoured, and the caller is told so in the result.
func (r *Registry) resolveDryRun(req mcp.CallToolRequest) (dryRun bool, forced bool) {
	requested := req.GetBool("dry_run", true)
	if r.Policy.DryRunOnly() && !requested {
		return true, true
	}
	return requested, false
}

// forcedDryRunNote tells the caller their commit was overridden. Without it a
// model would report a change that never actually happened.
const forcedDryRunNote = " (policy.dryRunOnly is set on this KAI instance, " +
	"so dry_run=false was overridden; no change was made)"

// annotate appends forcedDryRunNote when policy pinned the write to a dry run.
func annotate(result *k8s.WriteResult, forced bool) *k8s.WriteResult {
	if forced {
		result.Message += forcedDryRunNote
	}
	return result
}

func (r *Registry) scaleDeployment(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ns, err := req.RequireString("namespace")
	if err != nil {
		return resultErr(err), nil
	}
	name, err := req.RequireString("name")
	if err != nil {
		return resultErr(err), nil
	}
	replicas, err := req.RequireInt("replicas")
	if err != nil {
		return resultErr(err), nil
	}
	if replicas < 0 {
		return errorf("replicas must be zero or greater, got %d", replicas), nil
	}

	// Scaling goes through the scale subresource, so an operator can grant
	// "deployments/scale" without granting full deployment write access.
	if denied := r.guard(ctx, policy.Action{
		Verb: policy.VerbUpdate, Group: "apps", Resource: "deployments",
		Subresource: "scale", Namespace: ns, Name: name,
	}); denied != nil {
		return denied, nil
	}

	dryRun, forced := r.resolveDryRun(req)
	result, err := r.Client.ScaleDeployment(ctx, ns, name, int32(replicas), dryRun)
	if err != nil {
		return resultErr(err), nil
	}
	annotate(result, forced)
	return structured(result, result.Message), nil
}

func (r *Registry) restartDeployment(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ns, err := req.RequireString("namespace")
	if err != nil {
		return resultErr(err), nil
	}
	name, err := req.RequireString("name")
	if err != nil {
		return resultErr(err), nil
	}

	if denied := r.guard(ctx, policy.Action{
		Verb: policy.VerbPatch, Group: "apps", Resource: "deployments", Namespace: ns, Name: name,
	}); denied != nil {
		return denied, nil
	}

	dryRun, forced := r.resolveDryRun(req)
	result, err := r.Client.RestartDeployment(ctx, ns, name, dryRun)
	if err != nil {
		return resultErr(err), nil
	}
	annotate(result, forced)
	return structured(result, result.Message), nil
}

func (r *Registry) deletePod(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ns, err := req.RequireString("namespace")
	if err != nil {
		return resultErr(err), nil
	}
	name, err := req.RequireString("name")
	if err != nil {
		return resultErr(err), nil
	}

	if denied := r.guard(ctx, policy.Action{
		Verb: policy.VerbDelete, Resource: "pods", Namespace: ns, Name: name,
	}); denied != nil {
		return denied, nil
	}

	dryRun, forced := r.resolveDryRun(req)
	result, err := r.Client.DeletePod(ctx, ns, name, dryRun)
	if err != nil {
		return resultErr(err), nil
	}
	annotate(result, forced)
	return structured(result, result.Message), nil
}

func (r *Registry) cordonNode(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return r.setSchedulable(ctx, req, false)
}

func (r *Registry) uncordonNode(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return r.setSchedulable(ctx, req, true)
}

func (r *Registry) setSchedulable(ctx context.Context, req mcp.CallToolRequest, schedulable bool) (*mcp.CallToolResult, error) {
	name, err := req.RequireString("name")
	if err != nil {
		return resultErr(err), nil
	}

	if denied := r.guard(ctx, policy.Action{
		Verb: policy.VerbPatch, Resource: "nodes", Name: name,
	}); denied != nil {
		return denied, nil
	}

	dryRun, forced := r.resolveDryRun(req)
	result, err := r.Client.SetNodeSchedulable(ctx, name, schedulable, dryRun)
	if err != nil {
		return resultErr(err), nil
	}
	annotate(result, forced)
	return structured(result, result.Message), nil
}

func (r *Registry) applyManifest(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	manifest, err := req.RequireString("manifest")
	if err != nil {
		return resultErr(err), nil
	}
	ns, err := req.RequireString("namespace")
	if err != nil {
		return resultErr(err), nil
	}

	// Parse before guarding so each object's own kind is checked, rather than
	// approving a blanket "apply" and discovering the contents afterwards.
	objects, err := r.Client.PlanApply(manifest, ns)
	if err != nil {
		return resultErr(err), nil
	}
	if len(objects) == 0 {
		return errorf("manifest contains no objects"), nil
	}

	actions := make([]policy.Action, 0, len(objects))
	for _, o := range objects {
		actions = append(actions, policy.Action{
			Verb:      policy.VerbPatch, // server-side apply is a patch
			Group:     o.Group,
			Resource:  o.Resource,
			Namespace: o.Namespace,
			Name:      o.Name,
		})
	}
	if denied := r.guard(ctx, actions...); denied != nil {
		return denied, nil
	}

	dryRun, forced := r.resolveDryRun(req)
	result, err := r.Client.ApplyManifest(ctx, manifest, ns, dryRun)
	if err != nil {
		return resultErr(err), nil
	}
	if forced {
		result.Message += " (policy.dryRunOnly is set on this KAI instance, so dry_run=false was overridden; no change was made)"
	}

	summary := result.Message
	for _, o := range result.Objects {
		summary += fmt.Sprintf("\n  %s %s/%s: %s", o.Kind, o.Namespace, o.Name, o.Result)
	}
	return structured(result, summary), nil
}
