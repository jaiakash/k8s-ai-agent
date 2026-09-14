package tools

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jaiakash/k8s-ai-agent/internal/k8s"
	"github.com/jaiakash/k8s-ai-agent/internal/policy"
)

var readToolNames = []string{
	"cluster_info", "list_namespaces", "list_pods", "get_pod", "get_pod_logs",
	"list_events", "list_deployments", "list_services", "list_nodes",
	"get_resource", "top_pods", "top_nodes",
}

func (r *Registry) registerReadTools(s *server.MCPServer) {
	s.AddTool(mcp.NewTool("cluster_info",
		mcp.WithDescription("Summarize the cluster KAI is connected to: server version, node and namespace counts, and whether the metrics API is available. Call this first when you need orientation."),
		mcp.WithToolTitle("Cluster info"),
		readOnly(),
		mcp.WithOutputSchema[k8s.ClusterInfo](),
	), r.clusterInfo)

	s.AddTool(mcp.NewTool("list_namespaces",
		mcp.WithDescription("List namespaces visible to this agent, with their status and age. Use this to discover valid namespace values before calling a tool that needs one."),
		mcp.WithToolTitle("List namespaces"),
		readOnly(),
	), r.listNamespaces)

	s.AddTool(mcp.NewTool("list_pods",
		mcp.WithDescription("List pods with their health. Unhealthy pods are returned first and each carries an 'issue' field naming the problem (CrashLoopBackOff, ImagePullBackOff, Unschedulable, ...). Start troubleshooting here."),
		mcp.WithToolTitle("List pods"),
		readOnly(),
		mcp.WithString("namespace", mcp.Description(namespaceArg)),
		mcp.WithString("label_selector", mcp.Description("Label selector, e.g. \"app=frontend,tier!=cache\".")),
		mcp.WithString("field_selector", mcp.Description("Field selector, e.g. \"status.phase=Running\".")),
		mcp.WithNumber("limit", mcp.Description("Maximum pods to return. Defaults to 200."), mcp.Min(1), mcp.Max(1000)),
		mcp.WithOutputSchema[k8s.PodList](),
	), r.listPods)

	s.AddTool(mcp.NewTool("get_pod",
		mcp.WithDescription("Get full detail for one pod: container states, restart counts, the previous termination reason (for example OOMKilled), unmet conditions, and the pod's recent events. This is the single most useful call when a pod is misbehaving."),
		mcp.WithToolTitle("Get pod detail"),
		readOnly(),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Namespace containing the pod.")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Pod name.")),
		mcp.WithOutputSchema[k8s.PodDetail](),
	), r.getPod)

	s.AddTool(mcp.NewTool("get_pod_logs",
		mcp.WithDescription("Read container logs from a pod. Set previous=true to read the logs of the last crashed container, which is where a CrashLoopBackOff cause usually is."),
		mcp.WithToolTitle("Get pod logs"),
		readOnly(),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Namespace containing the pod.")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Pod name.")),
		mcp.WithString("container", mcp.Description("Container name. Required only for multi-container pods.")),
		mcp.WithNumber("tail_lines", mcp.Description("Number of lines from the end of the log. Defaults to 100."), mcp.Min(1)),
		mcp.WithBoolean("previous", mcp.Description("Read logs from the previous, crashed instance of the container."), mcp.DefaultBool(false)),
	), r.getPodLogs)

	s.AddTool(mcp.NewTool("list_events",
		mcp.WithDescription("List recent cluster events, warnings first. Events explain scheduling failures, image pull errors, probe failures and evictions that pod status alone does not."),
		mcp.WithToolTitle("List events"),
		readOnly(),
		mcp.WithString("namespace", mcp.Description(namespaceArg)),
		mcp.WithBoolean("warnings_only", mcp.Description("Return only Warning events."), mcp.DefaultBool(false)),
		mcp.WithNumber("limit", mcp.Description("Maximum events to return. Defaults to 50."), mcp.Min(1), mcp.Max(500)),
		mcp.WithOutputSchema[k8s.EventList](),
	), r.listEvents)

	s.AddTool(mcp.NewTool("list_deployments",
		mcp.WithDescription("List deployments with ready/desired replica counts, current images, and the reason any deployment is not fully available."),
		mcp.WithToolTitle("List deployments"),
		readOnly(),
		mcp.WithString("namespace", mcp.Description(namespaceArg)),
		mcp.WithString("label_selector", mcp.Description("Label selector, e.g. \"app=frontend\".")),
	), r.listDeployments)

	s.AddTool(mcp.NewTool("list_services",
		mcp.WithDescription("List services with their type, cluster IP, ports and pod selector. Use the selector to check a service actually targets the pods you expect."),
		mcp.WithToolTitle("List services"),
		readOnly(),
		mcp.WithString("namespace", mcp.Description(namespaceArg)),
	), r.listServices)

	s.AddTool(mcp.NewTool("list_nodes",
		mcp.WithDescription("List nodes with readiness, roles, kubelet version, capacity, cordon status and any active pressure conditions (memory, disk, PID)."),
		mcp.WithToolTitle("List nodes"),
		readOnly(),
	), r.listNodes)

	s.AddTool(mcp.NewTool("get_resource",
		mcp.WithDescription("Fetch any Kubernetes resource as YAML-equivalent JSON, including custom resources. Accepts short names (deploy, po, svc), plural names, or group-qualified names (deployments.apps). Use this for kinds without a dedicated tool."),
		mcp.WithToolTitle("Get resource"),
		readOnly(),
		mcp.WithString("resource", mcp.Required(), mcp.Description("Resource type: \"deployments\", \"deploy\", \"deployments.apps\", or a CRD's plural name.")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Object name.")),
		mcp.WithString("namespace", mcp.Description("Namespace, for namespaced resources.")),
	), r.getResource)

	s.AddTool(mcp.NewTool("top_pods",
		mcp.WithDescription("Show live pod CPU and memory usage, highest CPU first. Requires metrics-server; cluster_info reports whether it is available."),
		mcp.WithToolTitle("Top pods"),
		readOnly(),
		mcp.WithString("namespace", mcp.Description(namespaceArg)),
		mcp.WithNumber("limit", mcp.Description("Maximum rows to return. Defaults to 20."), mcp.Min(1), mcp.Max(200)),
	), r.topPods)

	s.AddTool(mcp.NewTool("top_nodes",
		mcp.WithDescription("Show live node CPU and memory usage as absolute figures and as a percentage of allocatable capacity. Requires metrics-server."),
		mcp.WithToolTitle("Top nodes"),
		readOnly(),
	), r.topNodes)
}

func (r *Registry) clusterInfo(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if denied := r.guard(ctx, policy.Action{Verb: policy.VerbList, Resource: "nodes"}); denied != nil {
		return denied, nil
	}

	info, err := r.Client.ClusterInfo(ctx)
	if err != nil {
		return resultErr(err), nil
	}

	summary := fmt.Sprintf("Cluster %s (%s)\n%d/%d nodes ready, %d namespaces\nMetrics API: %v",
		info.Context, info.ServerVersion, info.ReadyNodes, info.Nodes, info.Namespaces, info.MetricsAPI)
	return structured(info, summary), nil
}

func (r *Registry) listNamespaces(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if denied := r.guard(ctx, policy.Action{Verb: policy.VerbList, Resource: "namespaces"}); denied != nil {
		return denied, nil
	}

	list, err := r.Client.ListNamespaces(ctx)
	if err != nil {
		return resultErr(err), nil
	}

	rows := make([][]string, 0, len(list))
	for _, n := range list {
		rows = append(rows, []string{n.Name, n.Status, n.Age})
	}
	return structured(map[string]any{"namespaces": list, "count": len(list)},
		table([]string{"NAME", "STATUS", "AGE"}, rows)), nil
}

func (r *Registry) listPods(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ns := req.GetString("namespace", "")
	if denied := r.guard(ctx, policy.Action{Verb: policy.VerbList, Resource: "pods", Namespace: ns}); denied != nil {
		return denied, nil
	}

	limit := int64(req.GetInt("limit", 200))
	list, err := r.Client.ListPods(ctx, ns,
		req.GetString("label_selector", ""),
		req.GetString("field_selector", ""),
		limit)
	if err != nil {
		return resultErr(err), nil
	}

	rows := make([][]string, 0, len(list.Pods))
	for _, p := range list.Pods {
		rows = append(rows, []string{p.Name, p.Ready, p.Phase, strconv.Itoa(int(p.Restarts)), p.Age, p.Issue})
	}
	summary := fmt.Sprintf("%d pods in %s, %d unhealthy\n\n%s",
		list.Count, list.Namespace, list.Unhealthy,
		table([]string{"NAME", "READY", "STATUS", "RESTARTS", "AGE", "ISSUE"}, rows))
	if list.Truncated {
		summary += "\n(result truncated; raise limit or narrow with a selector)"
	}
	return structured(list, summary), nil
}

func (r *Registry) getPod(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ns, err := req.RequireString("namespace")
	if err != nil {
		return resultErr(err), nil
	}
	name, err := req.RequireString("name")
	if err != nil {
		return resultErr(err), nil
	}

	if denied := r.guard(ctx, policy.Action{
		Verb: policy.VerbGet, Resource: "pods", Namespace: ns, Name: name,
	}); denied != nil {
		return denied, nil
	}

	detail, err := r.Client.GetPod(ctx, ns, name)
	if err != nil {
		return resultErr(err), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s/%s  phase=%s  node=%s  age=%s\n", detail.Namespace, detail.Name, detail.Phase, detail.Node, detail.Age)
	if detail.Issue != "" {
		fmt.Fprintf(&sb, "issue: %s\n", detail.Issue)
	}
	for _, c := range detail.Containers {
		fmt.Fprintf(&sb, "container %s (%s): %s, restarts=%d\n", c.Name, c.Image, c.State, c.Restarts)
		if c.LastTerminated != "" {
			fmt.Fprintf(&sb, "  last terminated: %s\n", c.LastTerminated)
		}
	}
	for _, e := range detail.RecentEvents {
		fmt.Fprintf(&sb, "event %s %s: %s\n", e.Type, e.Reason, e.Message)
	}
	return structured(detail, sb.String()), nil
}

func (r *Registry) getPodLogs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ns, err := req.RequireString("namespace")
	if err != nil {
		return resultErr(err), nil
	}
	name, err := req.RequireString("name")
	if err != nil {
		return resultErr(err), nil
	}

	// Logs are a subresource with their own RBAC rule, so pre-flight the
	// subresource rather than plain pod read: a caller can hold one without
	// the other, and the denial message should say which is missing.
	if denied := r.guard(ctx, policy.Action{
		Verb: policy.VerbGet, Resource: "pods", Subresource: "log", Namespace: ns, Name: name,
	}); denied != nil {
		return denied, nil
	}

	tail := int64(req.GetInt("tail_lines", 100))
	if max := r.Policy.MaxLogLines(); tail > max {
		tail = max
	}

	logs, err := r.Client.PodLogs(ctx, ns, name, req.GetString("container", ""), tail, req.GetBool("previous", false))
	if err != nil {
		return resultErr(err), nil
	}
	return mcp.NewToolResultText(logs), nil
}

func (r *Registry) listEvents(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ns := req.GetString("namespace", "")
	if denied := r.guard(ctx, policy.Action{Verb: policy.VerbList, Resource: "events", Namespace: ns}); denied != nil {
		return denied, nil
	}

	list, err := r.Client.ListEvents(ctx, ns, int64(req.GetInt("limit", 50)), req.GetBool("warnings_only", false))
	if err != nil {
		return resultErr(err), nil
	}

	rows := make([][]string, 0, len(list.Events))
	for _, e := range list.Events {
		rows = append(rows, []string{e.Age, e.Type, e.Reason, e.Object, e.Message})
	}
	summary := fmt.Sprintf("%d events in %s (%d warnings)\n\n%s",
		list.Count, list.Namespace, list.Warnings,
		table([]string{"AGE", "TYPE", "REASON", "OBJECT", "MESSAGE"}, rows))
	return structured(list, summary), nil
}

func (r *Registry) listDeployments(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ns := req.GetString("namespace", "")
	if denied := r.guard(ctx, policy.Action{
		Verb: policy.VerbList, Group: "apps", Resource: "deployments", Namespace: ns,
	}); denied != nil {
		return denied, nil
	}

	list, err := r.Client.ListDeployments(ctx, ns, req.GetString("label_selector", ""))
	if err != nil {
		return resultErr(err), nil
	}

	rows := make([][]string, 0, len(list))
	for _, d := range list {
		rows = append(rows, []string{d.Namespace, d.Name, d.Ready, d.Age, d.Images, d.Issue})
	}
	return structured(map[string]any{"deployments": list, "count": len(list)},
		table([]string{"NAMESPACE", "NAME", "READY", "AGE", "IMAGES", "ISSUE"}, rows)), nil
}

func (r *Registry) listServices(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ns := req.GetString("namespace", "")
	if denied := r.guard(ctx, policy.Action{Verb: policy.VerbList, Resource: "services", Namespace: ns}); denied != nil {
		return denied, nil
	}

	list, err := r.Client.ListServices(ctx, ns)
	if err != nil {
		return resultErr(err), nil
	}

	rows := make([][]string, 0, len(list))
	for _, s := range list {
		rows = append(rows, []string{s.Namespace, s.Name, s.Type, s.ClusterIP, s.Ports, s.Selector})
	}
	return structured(map[string]any{"services": list, "count": len(list)},
		table([]string{"NAMESPACE", "NAME", "TYPE", "CLUSTER-IP", "PORTS", "SELECTOR"}, rows)), nil
}

func (r *Registry) listNodes(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if denied := r.guard(ctx, policy.Action{Verb: policy.VerbList, Resource: "nodes"}); denied != nil {
		return denied, nil
	}

	list, err := r.Client.ListNodes(ctx)
	if err != nil {
		return resultErr(err), nil
	}

	rows := make([][]string, 0, len(list))
	for _, n := range list {
		status := n.Status
		if !n.Schedulable {
			status += ",SchedulingDisabled"
		}
		rows = append(rows, []string{n.Name, status, n.Roles, n.Age, n.Version, strings.Join(n.Pressure, ",")})
	}
	return structured(map[string]any{"nodes": list, "count": len(list)},
		table([]string{"NAME", "STATUS", "ROLES", "AGE", "VERSION", "PRESSURE"}, rows)), nil
}

func (r *Registry) getResource(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resourceArg, err := req.RequireString("resource")
	if err != nil {
		return resultErr(err), nil
	}
	name, err := req.RequireString("name")
	if err != nil {
		return resultErr(err), nil
	}
	ns := req.GetString("namespace", "")

	gvr, namespaced, err := r.Client.ResolveResource(resourceArg)
	if err != nil {
		return resultErr(err), nil
	}
	if namespaced && ns == "" {
		return errorf("%s is namespaced; supply a namespace", gvr.Resource), nil
	}
	if !namespaced {
		ns = ""
	}

	// Guard against the *resolved* resource, not the user's spelling, so a
	// short name cannot smuggle a request past a policy written in full names.
	if denied := r.guard(ctx, policy.Action{
		Verb: policy.VerbGet, Group: gvr.Group, Resource: gvr.Resource, Namespace: ns, Name: name,
	}); denied != nil {
		return denied, nil
	}

	obj, err := r.Client.GetResource(ctx, gvr, namespaced, ns, name)
	if err != nil {
		return resultErr(err), nil
	}
	return structured(obj, fmt.Sprintf("%s %q retrieved", gvr.Resource, name)), nil
}

func (r *Registry) topPods(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ns := req.GetString("namespace", "")
	if denied := r.guard(ctx, policy.Action{
		Verb: policy.VerbList, Group: "metrics.k8s.io", Resource: "pods", Namespace: ns,
	}); denied != nil {
		return denied, nil
	}

	list, err := r.Client.TopPods(ctx, ns, req.GetInt("limit", 20))
	if err != nil {
		return resultErr(err), nil
	}

	rows := make([][]string, 0, len(list))
	for _, u := range list {
		rows = append(rows, []string{u.Namespace, u.Name, u.CPU, u.Memory})
	}
	return structured(map[string]any{"pods": list, "count": len(list)},
		table([]string{"NAMESPACE", "NAME", "CPU", "MEMORY"}, rows)), nil
}

func (r *Registry) topNodes(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if denied := r.guard(ctx, policy.Action{
		Verb: policy.VerbList, Group: "metrics.k8s.io", Resource: "nodes",
	}); denied != nil {
		return denied, nil
	}

	list, err := r.Client.TopNodes(ctx)
	if err != nil {
		return resultErr(err), nil
	}

	rows := make([][]string, 0, len(list))
	for _, u := range list {
		rows = append(rows, []string{u.Name, u.CPU, u.CPUPercent, u.Memory, u.MemoryPercent})
	}
	return structured(map[string]any{"nodes": list, "count": len(list)},
		table([]string{"NAME", "CPU", "CPU%", "MEMORY", "MEMORY%"}, rows)), nil
}
