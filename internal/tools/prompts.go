package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// MCP prompts are reusable investigation playbooks the host can offer as slash
// commands. They live here, on the server, rather than baked into one client:
// the knowledge of how to debug a CrashLoopBackOff belongs with the tools that
// do the debugging, and every host that connects gets it for free.
//
// Note what these are not: they do not tell a model how to format a reply or
// which flags to use. They sequence the tools. Anything the model should
// always know about this server belongs in the server instructions instead.
func (r *Registry) registerPrompts(s *server.MCPServer) {
	s.AddPrompt(mcp.NewPrompt("diagnose_pod",
		mcp.WithPromptDescription("Work out why a specific pod is not running, following the evidence from status to events to logs."),
		mcp.WithPromptTitle("Diagnose a pod"),
		mcp.WithArgument("namespace", mcp.ArgumentDescription("Namespace containing the pod."), mcp.RequiredArgument()),
		mcp.WithArgument("name", mcp.ArgumentDescription("Pod name."), mcp.RequiredArgument()),
	), r.diagnosePodPrompt)

	s.AddPrompt(mcp.NewPrompt("triage_namespace",
		mcp.WithPromptDescription("Survey a namespace for problems and report what is broken, in priority order."),
		mcp.WithPromptTitle("Triage a namespace"),
		mcp.WithArgument("namespace", mcp.ArgumentDescription("Namespace to triage."), mcp.RequiredArgument()),
	), r.triageNamespacePrompt)

	s.AddPrompt(mcp.NewPrompt("capacity_review",
		mcp.WithPromptDescription("Review cluster capacity and flag workloads whose resource requests look wrong."),
		mcp.WithPromptTitle("Review capacity"),
		mcp.WithArgument("namespace", mcp.ArgumentDescription("Namespace to review. Omit for the whole cluster.")),
	), r.capacityReviewPrompt)
}

func userPrompt(description, text string) *mcp.GetPromptResult {
	return mcp.NewGetPromptResult(description, []mcp.PromptMessage{
		mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent(text)),
	})
}

// arg reads a prompt argument, falling back to a default.
func arg(req mcp.GetPromptRequest, key, fallback string) string {
	if v, ok := req.Params.Arguments[key]; ok && strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

func (r *Registry) diagnosePodPrompt(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	ns := arg(req, "namespace", "")
	name := arg(req, "name", "")
	if ns == "" || name == "" {
		return nil, fmt.Errorf("diagnose_pod requires both namespace and name")
	}

	text := fmt.Sprintf(`Diagnose why pod %q in namespace %q is not healthy.

Work in this order and stop as soon as the evidence is conclusive:

1. Call get_pod for %s/%s. Read the container states, restart counts, and
   lastTerminated. An exit code of 137 with reason OOMKilled means the memory
   limit was hit; a non-zero application exit code means the process itself
   failed; ImagePullBackOff means the image name, tag or registry credentials
   are wrong; Unschedulable means no node can satisfy the pod's requests,
   affinity or tolerations.
2. Read the recentEvents already included in that response before fetching
   more. They usually name the cause outright.
3. If the container has restarted, call get_pod_logs with previous=true. The
   logs of the crashed instance hold the cause; the logs of the current one
   usually do not.
4. If the pod is Pending, call list_nodes and check whether capacity, cordon
   status or pressure conditions explain it.

Then report:
- The single root cause, stated plainly.
- The specific evidence you based it on, quoted.
- The fix, as a concrete change to the manifest or a specific command.

If the evidence is inconclusive, say so and name what you would need to look
at next. Do not guess at a cause the data does not support.`, name, ns, ns, name)

	return userPrompt(fmt.Sprintf("Diagnose pod %s/%s", ns, name), text), nil
}

func (r *Registry) triageNamespacePrompt(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	ns := arg(req, "namespace", "")
	if ns == "" {
		return nil, fmt.Errorf("triage_namespace requires a namespace")
	}

	text := fmt.Sprintf(`Triage namespace %q and report what is wrong.

1. Call list_pods for %s. The response sorts unhealthy pods first and gives
   each one an "issue" field; use the unhealthy count as your starting point.
2. Call list_deployments for %s to find workloads short of their desired
   replica count.
3. Call list_events for %s with warnings_only=true for failures that pod
   status alone does not explain, such as probe failures, evictions, failed
   scheduling and quota rejections.
4. Only call get_pod or get_pod_logs for pods that are actually broken. Do not
   walk every pod in the namespace.

Report the problems in order of user impact, most severe first. For each one
give the affected workload, the cause, and the fix. If nothing is wrong, say
so in one line rather than padding the answer.`, ns, ns, ns, ns)

	return userPrompt("Triage namespace "+ns, text), nil
}

func (r *Registry) capacityReviewPrompt(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	ns := arg(req, "namespace", "")
	scope := "the whole cluster"
	nsArg := ""
	if ns != "" {
		scope = "namespace " + ns
		nsArg = fmt.Sprintf(" for namespace %q", ns)
	}

	text := fmt.Sprintf(`Review resource capacity across %s.

1. Call top_nodes for actual node CPU and memory usage against allocatable
   capacity. If the metrics API is unavailable, say so and stop: without it
   the rest of this review is guesswork.
2. Call top_pods%s to find the heaviest consumers.
3. Call list_pods%s and look for pods that are Pending because of
   insufficient resources.
4. Use get_pod on the heaviest consumers to compare their actual usage with
   their configured requests and limits.

Report:
- Nodes above 80%% CPU or memory of allocatable.
- Pods whose usage far exceeds their requests, which will cause noisy-neighbour
  problems and bad scheduling decisions.
- Pods whose usage is far below their requests, which waste schedulable
  capacity.
- Pods with no requests set at all, which get scheduled essentially blind.

Give specific numbers for each finding. Recommend concrete request and limit
values rather than saying they should be "tuned".`, scope, nsArg, nsArg)

	return userPrompt("Capacity review: "+scope, text), nil
}
