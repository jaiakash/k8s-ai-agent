package k8s

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
)

// dynamicResource is the subset of the dynamic client both the namespaced and
// cluster-scoped interfaces satisfy, so apply can treat them uniformly.
type dynamicResource interface {
	Apply(ctx context.Context, name string, obj *unstructured.Unstructured, opts metav1.ApplyOptions, subresources ...string) (*unstructured.Unstructured, error)
}

var _ dynamicResource = dynamic.ResourceInterface(nil)

// fieldManager identifies KAI's writes in the object's managedFields, so an
// operator can see at a glance which fields an agent owns.
const fieldManager = "kai-mcp-server"

// WriteResult describes the outcome of a mutating operation.
//
// Before and After are reported explicitly because the caller is a language
// model relaying the change to a human: "scaled web from 2 to 5" is a far
// better thing to say than "ok".
type WriteResult struct {
	Action  string `json:"action"`
	Target  string `json:"target"`
	DryRun  bool   `json:"dryRun"`
	Before  string `json:"before,omitempty"`
	After   string `json:"after,omitempty"`
	Message string `json:"message"`
}

// dryRunOpts returns the API dry-run directive when dryRun is set.
//
// This is a *server-side* dry run: the API server runs admission, validation,
// quota checks and webhooks, then discards the write. It is a real rehearsal,
// unlike a client-side check that only inspects the manifest.
func dryRunOpts(dryRun bool) []string {
	if dryRun {
		return []string{metav1.DryRunAll}
	}
	return nil
}

func dryRunNote(dryRun bool) string {
	if dryRun {
		return " (dry run: validated by the API server, nothing was changed)"
	}
	return ""
}

// ScaleDeployment sets the replica count on a deployment.
func (c *Client) ScaleDeployment(ctx context.Context, ns, name string, replicas int32, dryRun bool) (*WriteResult, error) {
	target := fmt.Sprintf("deployment %s/%s", ns, name)

	current, err := c.Typed.AppsV1().Deployments(ns).GetScale(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, wrap("get scale for "+target, err)
	}
	before := current.Spec.Replicas

	if before == replicas && !dryRun {
		return &WriteResult{
			Action:  "scale",
			Target:  target,
			Before:  fmt.Sprintf("%d replicas", before),
			After:   fmt.Sprintf("%d replicas", replicas),
			Message: fmt.Sprintf("%s is already at %d replicas; no change made", target, replicas),
		}, nil
	}

	scale := &autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       autoscalingv1.ScaleSpec{Replicas: replicas},
	}
	if _, err := c.Typed.AppsV1().Deployments(ns).UpdateScale(ctx, name, scale, metav1.UpdateOptions{
		DryRun:       dryRunOpts(dryRun),
		FieldManager: fieldManager,
	}); err != nil {
		return nil, wrap("scale "+target, err)
	}

	return &WriteResult{
		Action:  "scale",
		Target:  target,
		DryRun:  dryRun,
		Before:  fmt.Sprintf("%d replicas", before),
		After:   fmt.Sprintf("%d replicas", replicas),
		Message: fmt.Sprintf("scaled %s from %d to %d replicas%s", target, before, replicas, dryRunNote(dryRun)),
	}, nil
}

// RestartDeployment performs a rolling restart.
//
// This mirrors `kubectl rollout restart`: stamping the pod template with a
// timestamp annotation makes the template hash change, which the deployment
// controller then rolls out normally. No pods are deleted directly.
func (c *Client) RestartDeployment(ctx context.Context, ns, name string, dryRun bool) (*WriteResult, error) {
	target := fmt.Sprintf("deployment %s/%s", ns, name)
	stamp := time.Now().UTC().Format(time.RFC3339)

	patch := fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`, stamp)

	if _, err := c.Typed.AppsV1().Deployments(ns).Patch(
		ctx, name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{
			DryRun:       dryRunOpts(dryRun),
			FieldManager: fieldManager,
		}); err != nil {
		return nil, wrap("restart "+target, err)
	}

	return &WriteResult{
		Action:  "restart",
		Target:  target,
		DryRun:  dryRun,
		After:   "restartedAt=" + stamp,
		Message: fmt.Sprintf("triggered a rolling restart of %s%s", target, dryRunNote(dryRun)),
	}, nil
}

// DeletePod deletes a single pod.
//
// A pod owned by a controller is recreated immediately; that is usually the
// intent (restart one replica), so the result says so rather than leaving the
// caller to wonder why the pod came back.
func (c *Client) DeletePod(ctx context.Context, ns, name string, dryRun bool) (*WriteResult, error) {
	target := fmt.Sprintf("pod %s/%s", ns, name)

	pod, err := c.Typed.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, wrap("get "+target, err)
	}

	owner := ""
	if len(pod.OwnerReferences) > 0 {
		owner = fmt.Sprintf(" it is managed by %s/%s and will be recreated",
			strings.ToLower(pod.OwnerReferences[0].Kind), pod.OwnerReferences[0].Name)
	}

	if err := c.Typed.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{
		DryRun: dryRunOpts(dryRun),
	}); err != nil {
		return nil, wrap("delete "+target, err)
	}

	return &WriteResult{
		Action:  "delete",
		Target:  target,
		DryRun:  dryRun,
		Before:  string(pod.Status.Phase),
		After:   "deleted",
		Message: fmt.Sprintf("deleted %s%s.%s", target, dryRunNote(dryRun), owner),
	}, nil
}

// SetNodeSchedulable cordons (schedulable=false) or uncordons a node.
//
// Cordon only stops *new* pods landing on the node; it does not evict what is
// already there. The message says so, because that distinction is the usual
// source of confusion during an incident.
func (c *Client) SetNodeSchedulable(ctx context.Context, name string, schedulable, dryRun bool) (*WriteResult, error) {
	target := "node " + name
	action := "cordon"
	if schedulable {
		action = "uncordon"
	}

	node, err := c.Typed.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, wrap("get "+target, err)
	}
	before := "schedulable"
	if node.Spec.Unschedulable {
		before = "cordoned"
	}

	patch := fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, !schedulable)
	if _, err := c.Typed.CoreV1().Nodes().Patch(
		ctx, name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{
			DryRun:       dryRunOpts(dryRun),
			FieldManager: fieldManager,
		}); err != nil {
		return nil, wrap(action+" "+target, err)
	}

	after := "cordoned"
	msg := fmt.Sprintf("cordoned %s: no new pods will be scheduled onto it, but existing pods keep running", target)
	if schedulable {
		after = "schedulable"
		msg = fmt.Sprintf("uncordoned %s: it will accept new pods again", target)
	}

	return &WriteResult{
		Action:  action,
		Target:  target,
		DryRun:  dryRun,
		Before:  before,
		After:   after,
		Message: msg + dryRunNote(dryRun),
	}, nil
}

// AppliedObject reports the outcome for one document in a manifest.
type AppliedObject struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Result    string `json:"result"`
}

// ApplyResult is the result of apply_manifest.
type ApplyResult struct {
	DryRun  bool            `json:"dryRun"`
	Objects []AppliedObject `json:"objects"`
	Message string          `json:"message"`
}

// PlannedObject is one object a manifest would write, resolved to a concrete
// API resource but not yet applied.
type PlannedObject struct {
	Kind      string `json:"kind"`
	Group     string `json:"group,omitempty"`
	Resource  string `json:"resource"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`

	obj        *unstructured.Unstructured
	gvr        schema.GroupVersionResource
	namespaced bool
}

// PlanApply parses a manifest and resolves every document to a real API
// resource without contacting the cluster to write anything.
//
// Applying is a two-step operation for a reason: the caller guards each
// resolved object against policy and RBAC *before* any of them is written. A
// single blanket "may apply" check would approve a manifest by its filename
// rather than by what it actually contains.
//
// defaultNamespace is used for namespaced objects that do not name one. An
// object naming a different namespace is rejected rather than quietly applied
// elsewhere, since the guard was evaluated against defaultNamespace.
func (c *Client) PlanApply(manifest, defaultNamespace string) ([]PlannedObject, error) {
	docs, err := splitManifest(manifest)
	if err != nil {
		return nil, err
	}

	out := make([]PlannedObject, 0, len(docs))
	for _, obj := range docs {
		gvk := obj.GroupVersionKind()
		if gvk.Kind == "" {
			return nil, fmt.Errorf("manifest document is missing kind")
		}
		if obj.GetName() == "" {
			return nil, fmt.Errorf("%s document is missing metadata.name", gvk.Kind)
		}

		mapping, err := c.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", gvk, err)
		}
		namespaced := mapping.Scope.Name() == meta.RESTScopeNameNamespace

		ns := obj.GetNamespace()
		switch {
		case !namespaced:
			ns = ""
			obj.SetNamespace("")
		case ns == "":
			ns = defaultNamespace
			obj.SetNamespace(ns)
		case defaultNamespace != "" && ns != defaultNamespace:
			return nil, fmt.Errorf(
				"%s %q declares namespace %q but the request targets %q; "+
					"apply it with namespace=%q so policy and RBAC are checked against the namespace actually written",
				gvk.Kind, obj.GetName(), ns, defaultNamespace, ns)
		}
		if namespaced && ns == "" {
			return nil, fmt.Errorf("%s %q is namespaced but no namespace was given", gvk.Kind, obj.GetName())
		}

		out = append(out, PlannedObject{
			Kind:       gvk.Kind,
			Group:      mapping.Resource.Group,
			Resource:   mapping.Resource.Resource,
			Namespace:  ns,
			Name:       obj.GetName(),
			obj:        obj,
			gvr:        mapping.Resource,
			namespaced: namespaced,
		})
	}
	return out, nil
}

// ApplyManifest server-side applies a YAML or JSON manifest, which may contain
// several documents separated by "---".
func (c *Client) ApplyManifest(ctx context.Context, manifest, defaultNamespace string, dryRun bool) (*ApplyResult, error) {
	plan, err := c.PlanApply(manifest, defaultNamespace)
	if err != nil {
		return nil, err
	}
	if len(plan) == 0 {
		return nil, fmt.Errorf("manifest contains no objects")
	}

	out := &ApplyResult{DryRun: dryRun, Objects: make([]AppliedObject, 0, len(plan))}
	for _, p := range plan {
		var target dynamicResource = c.Dynamic.Resource(p.gvr)
		if p.namespaced {
			target = c.Dynamic.Resource(p.gvr).Namespace(p.Namespace)
		}

		// Force resolves field-manager conflicts in KAI's favour. Without it
		// an apply fails whenever a human last edited the same field by hand,
		// which is the common case rather than the exception.
		applied, err := target.Apply(ctx, p.Name, p.obj, metav1.ApplyOptions{
			DryRun:       dryRunOpts(dryRun),
			FieldManager: fieldManager,
			Force:        true,
		})
		if err != nil {
			// Report what already succeeded: with several documents, knowing
			// where the manifest stopped is what makes it recoverable.
			return nil, fmt.Errorf("%w (applied %d of %d objects before this one)",
				wrap(fmt.Sprintf("apply %s %q", p.Kind, p.Name), err), len(out.Objects), len(plan))
		}

		out.Objects = append(out.Objects, AppliedObject{
			Kind:      p.Kind,
			Name:      applied.GetName(),
			Namespace: applied.GetNamespace(),
			Result:    "applied",
		})
	}

	out.Message = fmt.Sprintf("applied %d object(s)%s", len(out.Objects), dryRunNote(dryRun))
	return out, nil
}

// splitManifest decodes a multi-document YAML or JSON manifest.
func splitManifest(manifest string) ([]*unstructured.Unstructured, error) {
	reader := utilyaml.NewYAMLOrJSONDecoder(bytes.NewBufferString(manifest), 4096)

	var out []*unstructured.Unstructured
	for {
		raw := map[string]any{}
		err := reader.Decode(&raw)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse manifest: %w", err)
		}
		if len(raw) == 0 {
			continue // an empty document between separators
		}
		out = append(out, &unstructured.Unstructured{Object: raw})
	}
	return out, nil
}
