package k8s

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The result types below are deliberately flat and short-keyed. They are fed
// straight into a model's context window, so every redundant field is tokens
// spent on something the model did not ask for. Detail lives in the *Detail
// types, reached by a follow-up call.

// PodSummary is one row of a pod listing.
type PodSummary struct {
	Name     string `json:"name"`
	Phase    string `json:"phase"`
	Ready    string `json:"ready"` // "1/2"
	Restarts int32  `json:"restarts"`
	Age      string `json:"age"`
	Node     string `json:"node,omitempty"`
	// Issue names the reason a pod is not running, e.g. "CrashLoopBackOff"
	// or "ImagePullBackOff". Empty when the pod is healthy.
	Issue string `json:"issue,omitempty"`
}

// PodList is the result of list_pods.
type PodList struct {
	Namespace string       `json:"namespace"`
	Count     int          `json:"count"`
	Unhealthy int          `json:"unhealthy"`
	Pods      []PodSummary `json:"pods"`
	// Truncated reports that more pods exist than were returned.
	Truncated bool `json:"truncated,omitempty"`
}

// ListPods returns pods in ns, or across all namespaces when ns is empty.
func (c *Client) ListPods(ctx context.Context, ns, labelSelector, fieldSelector string, limit int64) (*PodList, error) {
	opts := metav1.ListOptions{
		LabelSelector: labelSelector,
		FieldSelector: fieldSelector,
	}
	if limit > 0 {
		opts.Limit = limit
	}

	pods, err := c.Typed.CoreV1().Pods(ns).List(ctx, opts)
	if err != nil {
		return nil, wrap("list pods", err)
	}

	out := &PodList{Namespace: nsLabel(ns), Pods: make([]PodSummary, 0, len(pods.Items))}
	for i := range pods.Items {
		s := summarizePod(&pods.Items[i])
		if s.Issue != "" {
			out.Unhealthy++
		}
		out.Pods = append(out.Pods, s)
	}
	out.Count = len(out.Pods)
	out.Truncated = pods.Continue != ""

	// Surface broken pods first: that is almost always why the model is
	// looking, and it survives truncation in a long listing.
	sort.SliceStable(out.Pods, func(i, j int) bool {
		return out.Pods[i].Issue != "" && out.Pods[j].Issue == ""
	})
	return out, nil
}

func summarizePod(p *corev1.Pod) PodSummary {
	var ready, restarts int32
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
		restarts += cs.RestartCount
	}

	s := PodSummary{
		Name:     p.Name,
		Phase:    string(p.Status.Phase),
		Ready:    fmt.Sprintf("%d/%d", ready, len(p.Spec.Containers)),
		Restarts: restarts,
		Age:      age(p.CreationTimestamp),
		Node:     p.Spec.NodeName,
		Issue:    podIssue(p),
	}
	return s
}

// podIssue names why a pod is not serving traffic, preferring the most
// specific signal available: a container's waiting reason beats the pod phase.
func podIssue(p *corev1.Pod) string {
	if p.DeletionTimestamp != nil {
		return "Terminating"
	}

	for _, cs := range append(p.Status.InitContainerStatuses, p.Status.ContainerStatuses...) {
		if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" {
			return w.Reason
		}
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
			return fmt.Sprintf("%s (exit %d)", t.Reason, t.ExitCode)
		}
	}

	switch p.Status.Phase {
	case corev1.PodSucceeded:
		return ""
	case corev1.PodRunning:
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status != corev1.ConditionTrue {
				if cond.Reason != "" {
					return cond.Reason
				}
				return "NotReady"
			}
		}
		return ""
	case corev1.PodPending:
		// An unschedulable pod is the single most common "why is nothing
		// happening" case, so report the scheduler's message verbatim.
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status != corev1.ConditionTrue {
				if cond.Message != "" {
					return fmt.Sprintf("%s: %s", cond.Reason, cond.Message)
				}
				return cond.Reason
			}
		}
		return "Pending"
	default:
		return string(p.Status.Phase)
	}
}

// ContainerDetail describes one container inside a pod.
type ContainerDetail struct {
	Name     string `json:"name"`
	Image    string `json:"image"`
	Ready    bool   `json:"ready"`
	Restarts int32  `json:"restarts"`
	State    string `json:"state"`
	// LastTerminated explains the previous crash, when there was one. This is
	// usually the key fact in a CrashLoopBackOff investigation.
	LastTerminated string `json:"lastTerminated,omitempty"`
	Requests       string `json:"requests,omitempty"`
	Limits         string `json:"limits,omitempty"`
}

// PodDetail is the result of get_pod.
type PodDetail struct {
	Name           string            `json:"name"`
	Namespace      string            `json:"namespace"`
	Phase          string            `json:"phase"`
	Node           string            `json:"node,omitempty"`
	PodIP          string            `json:"podIP,omitempty"`
	Age            string            `json:"age"`
	Issue          string            `json:"issue,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	OwnerKind      string            `json:"ownerKind,omitempty"`
	OwnerName      string            `json:"ownerName,omitempty"`
	Containers     []ContainerDetail `json:"containers"`
	InitContainers []ContainerDetail `json:"initContainers,omitempty"`
	Conditions     []string          `json:"conditions,omitempty"`
	// RecentEvents are the pod's own events, newest first.
	RecentEvents []EventSummary `json:"recentEvents,omitempty"`
}

// GetPod returns detail for a single pod, including its recent events.
func (c *Client) GetPod(ctx context.Context, ns, name string) (*PodDetail, error) {
	p, err := c.Typed.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, wrap(fmt.Sprintf("get pod %s/%s", ns, name), err)
	}

	d := &PodDetail{
		Name:       p.Name,
		Namespace:  p.Namespace,
		Phase:      string(p.Status.Phase),
		Node:       p.Spec.NodeName,
		PodIP:      p.Status.PodIP,
		Age:        age(p.CreationTimestamp),
		Issue:      podIssue(p),
		Labels:     p.Labels,
		Containers: containerDetails(p.Spec.Containers, p.Status.ContainerStatuses),
	}
	d.InitContainers = containerDetails(p.Spec.InitContainers, p.Status.InitContainerStatuses)

	if len(p.OwnerReferences) > 0 {
		d.OwnerKind = p.OwnerReferences[0].Kind
		d.OwnerName = p.OwnerReferences[0].Name
	}
	for _, cond := range p.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			d.Conditions = append(d.Conditions, fmt.Sprintf("%s=%s %s %s", cond.Type, cond.Status, cond.Reason, cond.Message))
		}
	}

	// Events are best-effort: the caller may hold pod read access without
	// event access, and a partial answer beats a hard failure here.
	if ev, err := c.podEvents(ctx, ns, name); err == nil {
		d.RecentEvents = ev
	}
	return d, nil
}

func containerDetails(specs []corev1.Container, statuses []corev1.ContainerStatus) []ContainerDetail {
	byName := make(map[string]corev1.ContainerStatus, len(statuses))
	for _, cs := range statuses {
		byName[cs.Name] = cs
	}

	out := make([]ContainerDetail, 0, len(specs))
	for _, spec := range specs {
		d := ContainerDetail{Name: spec.Name, Image: spec.Image, State: "unknown"}
		if r := resourceList(spec.Resources.Requests); r != "" {
			d.Requests = r
		}
		if l := resourceList(spec.Resources.Limits); l != "" {
			d.Limits = l
		}

		if cs, ok := byName[spec.Name]; ok {
			d.Ready = cs.Ready
			d.Restarts = cs.RestartCount
			switch {
			case cs.State.Running != nil:
				d.State = "running"
			case cs.State.Waiting != nil:
				d.State = "waiting: " + cs.State.Waiting.Reason
				if cs.State.Waiting.Message != "" {
					d.State += " (" + cs.State.Waiting.Message + ")"
				}
			case cs.State.Terminated != nil:
				d.State = fmt.Sprintf("terminated: %s (exit %d)", cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
			}
			if lt := cs.LastTerminationState.Terminated; lt != nil {
				d.LastTerminated = fmt.Sprintf("%s (exit %d) at %s", lt.Reason, lt.ExitCode, lt.FinishedAt.Format(time.RFC3339))
			}
		}
		out = append(out, d)
	}
	return out
}

func resourceList(rl corev1.ResourceList) string {
	if len(rl) == 0 {
		return ""
	}
	parts := make([]string, 0, len(rl))
	for _, key := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		if q, ok := rl[key]; ok {
			parts = append(parts, fmt.Sprintf("%s=%s", key, q.String()))
		}
	}
	return strings.Join(parts, " ")
}

// EventSummary is one Kubernetes event.
type EventSummary struct {
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Object  string `json:"object,omitempty"`
	Message string `json:"message"`
	Count   int32  `json:"count,omitempty"`
	Age     string `json:"age"`
}

// EventList is the result of list_events.
type EventList struct {
	Namespace string         `json:"namespace"`
	Count     int            `json:"count"`
	Warnings  int            `json:"warnings"`
	Events    []EventSummary `json:"events"`
}

// ListEvents returns recent events, warnings first.
func (c *Client) ListEvents(ctx context.Context, ns string, limit int64, warningsOnly bool) (*EventList, error) {
	opts := metav1.ListOptions{}
	if warningsOnly {
		opts.FieldSelector = "type=Warning"
	}

	events, err := c.Typed.CoreV1().Events(ns).List(ctx, opts)
	if err != nil {
		return nil, wrap("list events", err)
	}

	out := &EventList{Namespace: nsLabel(ns)}
	summaries := make([]EventSummary, 0, len(events.Items))
	for i := range events.Items {
		e := &events.Items[i]
		if e.Type == corev1.EventTypeWarning {
			out.Warnings++
		}
		summaries = append(summaries, summarizeEvent(e))
	}

	// Warnings first, then newest first. A model reading a truncated list
	// should see the problems, not the routine Scheduled/Pulled chatter.
	sort.SliceStable(summaries, func(i, j int) bool {
		iw := summaries[i].Type == corev1.EventTypeWarning
		jw := summaries[j].Type == corev1.EventTypeWarning
		if iw != jw {
			return iw
		}
		return eventOrder(events.Items, i) > eventOrder(events.Items, j)
	})

	if limit > 0 && int64(len(summaries)) > limit {
		summaries = summaries[:limit]
	}
	out.Events = summaries
	out.Count = len(summaries)
	return out, nil
}

// eventOrder returns a sortable timestamp for the i'th event.
func eventOrder(items []corev1.Event, i int) int64 {
	if i >= len(items) {
		return 0
	}
	return eventTime(&items[i]).Unix()
}

func eventTime(e *corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

func summarizeEvent(e *corev1.Event) EventSummary {
	return EventSummary{
		Type:    e.Type,
		Reason:  e.Reason,
		Object:  fmt.Sprintf("%s/%s", strings.ToLower(e.InvolvedObject.Kind), e.InvolvedObject.Name),
		Message: e.Message,
		Count:   e.Count,
		Age:     age(metav1.NewTime(eventTime(e))),
	}
}

func (c *Client) podEvents(ctx context.Context, ns, name string) ([]EventSummary, error) {
	events, err := c.Typed.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s", name),
	})
	if err != nil {
		return nil, err
	}

	out := make([]EventSummary, 0, len(events.Items))
	for i := range events.Items {
		out = append(out, summarizeEvent(&events.Items[i]))
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Type == corev1.EventTypeWarning && out[j].Type != corev1.EventTypeWarning
	})
	if len(out) > 10 {
		out = out[:10]
	}
	return out, nil
}

// PodLogs returns container logs, capped at tail lines.
func (c *Client) PodLogs(ctx context.Context, ns, name, container string, tail int64, previous bool) (string, error) {
	opts := &corev1.PodLogOptions{
		Container: container,
		Previous:  previous,
	}
	if tail > 0 {
		opts.TailLines = &tail
	}

	stream, err := c.Typed.CoreV1().Pods(ns).GetLogs(name, opts).Stream(ctx)
	if err != nil {
		return "", wrap(fmt.Sprintf("get logs for %s/%s", ns, name), err)
	}
	defer stream.Close()

	var sb strings.Builder
	if _, err := copyLimited(&sb, stream, maxLogBytes); err != nil {
		return "", fmt.Errorf("read logs for %s/%s: %w", ns, name, err)
	}
	if sb.Len() == 0 {
		return "(no log output)", nil
	}
	return sb.String(), nil
}

// DeploymentSummary is one row of a deployment listing.
type DeploymentSummary struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Ready     string `json:"ready"` // "2/3"
	UpToDate  int32  `json:"upToDate"`
	Available int32  `json:"available"`
	Age       string `json:"age"`
	Images    string `json:"images,omitempty"`
	Issue     string `json:"issue,omitempty"`
}

// ListDeployments returns deployments in ns, or all namespaces when empty.
func (c *Client) ListDeployments(ctx context.Context, ns, labelSelector string) ([]DeploymentSummary, error) {
	list, err := c.Typed.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, wrap("list deployments", err)
	}

	out := make([]DeploymentSummary, 0, len(list.Items))
	for i := range list.Items {
		d := &list.Items[i]

		var desired int32 = 1
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}

		images := make([]string, 0, len(d.Spec.Template.Spec.Containers))
		for _, ctr := range d.Spec.Template.Spec.Containers {
			images = append(images, ctr.Image)
		}

		s := DeploymentSummary{
			Name:      d.Name,
			Namespace: d.Namespace,
			Ready:     fmt.Sprintf("%d/%d", d.Status.ReadyReplicas, desired),
			UpToDate:  d.Status.UpdatedReplicas,
			Available: d.Status.AvailableReplicas,
			Age:       age(d.CreationTimestamp),
			Images:    strings.Join(images, ","),
		}
		if d.Status.ReadyReplicas < desired {
			s.Issue = deploymentIssue(d)
		}
		out = append(out, s)
	}
	return out, nil
}

func deploymentIssue(d *appsv1.Deployment) string {
	for _, cond := range d.Status.Conditions {
		if cond.Status != "True" && cond.Message != "" {
			return fmt.Sprintf("%s: %s", cond.Reason, cond.Message)
		}
	}
	return "not all replicas ready"
}

// NodeSummary is one row of a node listing.
type NodeSummary struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Roles       string `json:"roles,omitempty"`
	Age         string `json:"age"`
	Version     string `json:"version"`
	Schedulable bool   `json:"schedulable"`
	// Pressure lists any active node pressure conditions (disk, memory, PID).
	Pressure []string `json:"pressure,omitempty"`
	CPU      string   `json:"cpuCapacity,omitempty"`
	Memory   string   `json:"memoryCapacity,omitempty"`
}

// ListNodes returns every node with its health conditions.
func (c *Client) ListNodes(ctx context.Context) ([]NodeSummary, error) {
	list, err := c.Typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, wrap("list nodes", err)
	}

	out := make([]NodeSummary, 0, len(list.Items))
	for i := range list.Items {
		n := &list.Items[i]
		s := NodeSummary{
			Name:        n.Name,
			Status:      "NotReady",
			Age:         age(n.CreationTimestamp),
			Version:     n.Status.NodeInfo.KubeletVersion,
			Schedulable: !n.Spec.Unschedulable,
		}

		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				s.Status = "Ready"
				continue
			}
			// Any non-Ready condition that is True is a pressure signal.
			if cond.Type != corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				s.Pressure = append(s.Pressure, string(cond.Type))
			}
		}

		var roles []string
		for label := range n.Labels {
			if role, ok := strings.CutPrefix(label, "node-role.kubernetes.io/"); ok && role != "" {
				roles = append(roles, role)
			}
		}
		sort.Strings(roles)
		s.Roles = strings.Join(roles, ",")

		if q, ok := n.Status.Capacity[corev1.ResourceCPU]; ok {
			s.CPU = q.String()
		}
		if q, ok := n.Status.Capacity[corev1.ResourceMemory]; ok {
			s.Memory = q.String()
		}
		out = append(out, s)
	}
	return out, nil
}

// ServiceSummary is one row of a service listing.
type ServiceSummary struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Type      string `json:"type"`
	ClusterIP string `json:"clusterIP,omitempty"`
	Ports     string `json:"ports,omitempty"`
	Selector  string `json:"selector,omitempty"`
	Age       string `json:"age"`
}

// ListServices returns services in ns, or all namespaces when empty.
func (c *Client) ListServices(ctx context.Context, ns string) ([]ServiceSummary, error) {
	list, err := c.Typed.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, wrap("list services", err)
	}

	out := make([]ServiceSummary, 0, len(list.Items))
	for i := range list.Items {
		s := &list.Items[i]

		ports := make([]string, 0, len(s.Spec.Ports))
		for _, p := range s.Spec.Ports {
			ports = append(ports, fmt.Sprintf("%d/%s", p.Port, p.Protocol))
		}
		selectors := make([]string, 0, len(s.Spec.Selector))
		for k, v := range s.Spec.Selector {
			selectors = append(selectors, k+"="+v)
		}
		sort.Strings(selectors)

		out = append(out, ServiceSummary{
			Name:      s.Name,
			Namespace: s.Namespace,
			Type:      string(s.Spec.Type),
			ClusterIP: s.Spec.ClusterIP,
			Ports:     strings.Join(ports, ","),
			Selector:  strings.Join(selectors, ","),
			Age:       age(s.CreationTimestamp),
		})
	}
	return out, nil
}

// NamespaceSummary is one row of a namespace listing.
type NamespaceSummary struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Age    string `json:"age"`
}

// ListNamespaces returns every namespace the caller can see.
func (c *Client) ListNamespaces(ctx context.Context) ([]NamespaceSummary, error) {
	list, err := c.Typed.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, wrap("list namespaces", err)
	}

	out := make([]NamespaceSummary, 0, len(list.Items))
	for i := range list.Items {
		n := &list.Items[i]
		out = append(out, NamespaceSummary{
			Name:   n.Name,
			Status: string(n.Status.Phase),
			Age:    age(n.CreationTimestamp),
		})
	}
	return out, nil
}

// GetResource fetches any resource as an unstructured object, CRDs included.
func (c *Client) GetResource(ctx context.Context, gvr schema.GroupVersionResource, namespaced bool, ns, name string) (map[string]any, error) {
	var ri = c.Dynamic.Resource(gvr).Namespace(ns)
	if !namespaced {
		ri = c.Dynamic.Resource(gvr)
	}

	obj, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, wrap(fmt.Sprintf("get %s %s", gvr.Resource, name), err)
	}

	// managedFields is pure bookkeeping and frequently larger than the spec
	// it describes. Stripping it keeps the model's context on the object.
	out := obj.DeepCopy().Object
	if md, ok := out["metadata"].(map[string]any); ok {
		delete(md, "managedFields")
	}
	return out, nil
}

// ClusterInfo is the result of the cluster_info tool and k8s://cluster/info.
type ClusterInfo struct {
	Context       string `json:"context,omitempty"`
	Host          string `json:"host,omitempty"`
	ServerVersion string `json:"serverVersion"`
	Platform      string `json:"platform,omitempty"`
	Nodes         int    `json:"nodes"`
	ReadyNodes    int    `json:"readyNodes"`
	Namespaces    int    `json:"namespaces"`
	MetricsAPI    bool   `json:"metricsApiAvailable"`
}

// ClusterInfo summarizes what KAI is connected to.
func (c *Client) ClusterInfo(ctx context.Context) (*ClusterInfo, error) {
	info := &ClusterInfo{Context: c.ContextName, Host: c.Host}

	version, err := c.Discovery.ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("discover server version: %w", err)
	}
	info.ServerVersion = version.GitVersion
	info.Platform = version.Platform

	if nodes, err := c.ListNodes(ctx); err == nil {
		info.Nodes = len(nodes)
		for _, n := range nodes {
			if n.Status == "Ready" {
				info.ReadyNodes++
			}
		}
	}
	if namespaces, err := c.ListNamespaces(ctx); err == nil {
		info.Namespaces = len(namespaces)
	}

	// Probe rather than assume: metrics-server is an add-on, and top_* should
	// say so up front instead of failing later.
	if _, err := c.Metrics.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{Limit: 1}); err == nil {
		info.MetricsAPI = true
	}
	return info, nil
}

// maxLogBytes caps a single log fetch. TailLines already bounds the line
// count, but a single pathological line (a stack trace, a base64 blob) can
// still blow past a model's context window, so bound the bytes too.
const maxLogBytes = 256 * 1024

// copyLimited copies at most limit bytes from src, appending a marker when it
// truncates so the model knows the logs are incomplete.
func copyLimited(dst *strings.Builder, src io.Reader, limit int64) (int64, error) {
	n, err := io.Copy(dst, io.LimitReader(src, limit))
	if err != nil {
		return n, err
	}
	if n == limit {
		dst.WriteString("\n... (truncated: log output exceeded 256KiB; narrow the range with tail_lines or container)")
	}
	return n, nil
}

// wrap turns an API error into a message a model can act on, rather than a
// bare status code.
func wrap(op string, err error) error {
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%s: not found", op)
	case apierrors.IsForbidden(err):
		return fmt.Errorf("%s: forbidden — the credentials this server uses lack the required RBAC permission", op)
	case apierrors.IsUnauthorized(err):
		return fmt.Errorf("%s: unauthorized — the server's credentials are invalid or expired", op)
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return fmt.Errorf("%s: the API server timed out", op)
	default:
		return fmt.Errorf("%s: %w", op, err)
	}
}

func nsLabel(ns string) string {
	if ns == "" {
		return "(all namespaces)"
	}
	return ns
}

// age renders a duration the way kubectl does: the two most significant units.
func age(t metav1.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t.Time)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}
