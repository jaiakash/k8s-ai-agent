package k8s

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func deployment(name, ns string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
			},
		},
	}
}

// withScaleSubresource teaches the fake clientset to serve deployments/scale.
// The fake's object tracker stores Deployments, not Scales, so GetScale panics
// without this. Reactors also let the test assert that KAI goes through the
// scale subresource, which is what makes a narrow "deployments/scale" RBAC
// grant sufficient to run the tool.
func withScaleSubresource(cs *fake.Clientset, ns, name string, replicas int32, updated *int32) {
	scale := &autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       autoscalingv1.ScaleSpec{Replicas: replicas},
	}

	cs.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "scale" {
			return false, nil, nil
		}
		return true, scale.DeepCopy(), nil
	})
	cs.PrependReactor("update", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "scale" {
			return false, nil, nil
		}
		obj := action.(k8stesting.UpdateActionImpl).Object.(*autoscalingv1.Scale)
		if updated != nil {
			*updated = obj.Spec.Replicas
		}
		return true, obj, nil
	})
}

// captureDryRun records the dry-run directive the client sent for verb/resource.
// The fake clientset does not implement dry-run semantics, so asserting the
// directive reaches the API is the meaningful check: on a real server that
// directive is what makes the write a rehearsal.
func captureDryRun(cs *fake.Clientset, verb, resource string, got *[]string, seen *bool) {
	cs.PrependReactor(verb, resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		*seen = true
		switch a := action.(type) {
		case k8stesting.DeleteActionImpl:
			*got = a.DeleteOptions.DryRun
		case k8stesting.UpdateActionImpl:
			*got = a.UpdateOptions.DryRun
		case k8stesting.PatchActionImpl:
			*got = a.PatchOptions.DryRun
		}
		return false, nil, nil // fall through to the tracker
	})
}

func TestScaleDeploymentReportsBeforeAndAfter(t *testing.T) {
	c, cs := testClient(deployment("web", "default", 2))
	var applied int32
	withScaleSubresource(cs, "default", "web", 2, &applied)

	got, err := c.ScaleDeployment(context.Background(), "default", "web", 5, false)
	if err != nil {
		t.Fatalf("ScaleDeployment: %v", err)
	}
	if applied != 5 {
		t.Errorf("replicas written = %d, want 5", applied)
	}
	if got.Before != "2 replicas" || got.After != "5 replicas" {
		t.Errorf("before/after = %q/%q, want 2/5 replicas", got.Before, got.After)
	}
	if !strings.Contains(got.Message, "from 2 to 5") {
		t.Errorf("message should state the transition, got %q", got.Message)
	}
	if got.DryRun {
		t.Error("DryRun should be false")
	}
}

func TestScaleDeploymentDryRunPropagatesDirective(t *testing.T) {
	c, cs := testClient(deployment("web", "default", 2))
	withScaleSubresource(cs, "default", "web", 2, nil)

	var dryRun []string
	var seen bool
	captureDryRun(cs, "update", "deployments", &dryRun, &seen)

	got, err := c.ScaleDeployment(context.Background(), "default", "web", 5, true)
	if err != nil {
		t.Fatalf("ScaleDeployment: %v", err)
	}
	if !seen {
		t.Fatal("expected an update call to reach the API")
	}
	if len(dryRun) != 1 || dryRun[0] != metav1.DryRunAll {
		t.Errorf("DryRun = %v, want [%s]", dryRun, metav1.DryRunAll)
	}
	if !got.DryRun {
		t.Error("result should be marked as a dry run")
	}
	if !strings.Contains(got.Message, "nothing was changed") {
		t.Errorf("dry-run message must make clear nothing changed, got %q", got.Message)
	}
}

func TestScaleDeploymentNoopWhenAlreadyAtTarget(t *testing.T) {
	c, cs := testClient(deployment("web", "default", 3))
	withScaleSubresource(cs, "default", "web", 3, nil)

	got, err := c.ScaleDeployment(context.Background(), "default", "web", 3, false)
	if err != nil {
		t.Fatalf("ScaleDeployment: %v", err)
	}
	if !strings.Contains(got.Message, "already at 3") {
		t.Errorf("want a no-op message, got %q", got.Message)
	}
}

func TestScaleDeploymentMissingIsReadable(t *testing.T) {
	c, _ := testClient()

	_, err := c.ScaleDeployment(context.Background(), "default", "ghost", 3, false)
	if err == nil {
		t.Fatal("want an error for a missing deployment")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should say not found, got %v", err)
	}
}

func TestRestartDeploymentStampsTemplate(t *testing.T) {
	c, cs := testClient(deployment("web", "default", 2))

	var patch []byte
	cs.PrependReactor("patch", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patch = action.(k8stesting.PatchActionImpl).Patch
		return false, nil, nil
	})

	got, err := c.RestartDeployment(context.Background(), "default", "web", false)
	if err != nil {
		t.Fatalf("RestartDeployment: %v", err)
	}
	// A rolling restart must change the pod template, not delete pods.
	if !strings.Contains(string(patch), "restartedAt") {
		t.Errorf("patch should stamp restartedAt on the template, got %s", patch)
	}
	if !strings.Contains(string(patch), `"template"`) {
		t.Errorf("patch must target spec.template so the controller rolls out, got %s", patch)
	}
	if !strings.Contains(got.Message, "rolling restart") {
		t.Errorf("message = %q", got.Message)
	}
}

func TestDeletePodDryRunPropagatesDirective(t *testing.T) {
	c, cs := testClient(pod("web", "default", nil))

	var dryRun []string
	var seen bool
	captureDryRun(cs, "delete", "pods", &dryRun, &seen)

	got, err := c.DeletePod(context.Background(), "default", "web", true)
	if err != nil {
		t.Fatalf("DeletePod: %v", err)
	}
	if !seen {
		t.Fatal("expected a delete call to reach the API")
	}
	if len(dryRun) != 1 || dryRun[0] != metav1.DryRunAll {
		t.Errorf("DryRun = %v, want [%s]", dryRun, metav1.DryRunAll)
	}
	if !got.DryRun {
		t.Error("result should be marked as a dry run")
	}
}

func TestDeletePodWarnsThatControllerRecreatesIt(t *testing.T) {
	p := pod("web", "default", func(p *corev1.Pod) {
		p.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc"}}
	})
	c, _ := testClient(p)

	got, err := c.DeletePod(context.Background(), "default", "web", false)
	if err != nil {
		t.Fatalf("DeletePod: %v", err)
	}
	if !strings.Contains(got.Message, "recreated") {
		t.Errorf("message should warn the pod comes back, got %q", got.Message)
	}
	if !strings.Contains(got.Message, "web-abc") {
		t.Errorf("message should name the owning controller, got %q", got.Message)
	}
}

func TestCordonExplainsItDoesNotEvict(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}
	c, _ := testClient(n)

	got, err := c.SetNodeSchedulable(context.Background(), "node-1", false, false)
	if err != nil {
		t.Fatalf("SetNodeSchedulable: %v", err)
	}
	if got.Action != "cordon" {
		t.Errorf("Action = %q, want cordon", got.Action)
	}
	// This distinction trips people up mid-incident; the tool must state it.
	if !strings.Contains(got.Message, "existing pods keep running") {
		t.Errorf("message should clarify cordon does not evict, got %q", got.Message)
	}
}

func TestUncordonRestoresScheduling(t *testing.T) {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Spec:       corev1.NodeSpec{Unschedulable: true},
	}
	c, cs := testClient(n)

	var patch []byte
	cs.PrependReactor("patch", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patch = action.(k8stesting.PatchActionImpl).Patch
		return false, nil, nil
	})

	got, err := c.SetNodeSchedulable(context.Background(), "node-1", true, false)
	if err != nil {
		t.Fatalf("SetNodeSchedulable: %v", err)
	}
	if got.Before != "cordoned" || got.After != "schedulable" {
		t.Errorf("before/after = %q/%q, want cordoned/schedulable", got.Before, got.After)
	}
	if !strings.Contains(string(patch), `"unschedulable":false`) {
		t.Errorf("patch should clear unschedulable, got %s", patch)
	}
}

func TestSplitManifestHandlesMultipleDocuments(t *testing.T) {
	manifest := `
apiVersion: v1
kind: ConfigMap
metadata:
  name: a
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: b
`
	objs, err := splitManifest(manifest)
	if err != nil {
		t.Fatalf("splitManifest: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("want 2 documents, got %d", len(objs))
	}
	if objs[0].GetName() != "a" || objs[1].GetName() != "b" {
		t.Errorf("names = %q, %q; want a, b", objs[0].GetName(), objs[1].GetName())
	}
}

func TestSplitManifestRejectsGarbage(t *testing.T) {
	if _, err := splitManifest("this: is: not: valid: yaml:"); err == nil {
		t.Fatal("want a parse error for malformed YAML")
	}
}

func TestDryRunOptsOnlySetWhenRequested(t *testing.T) {
	if got := dryRunOpts(false); got != nil {
		t.Errorf("dryRunOpts(false) = %v, want nil", got)
	}
	if got := dryRunOpts(true); len(got) != 1 || got[0] != metav1.DryRunAll {
		t.Errorf("dryRunOpts(true) = %v, want [%s]", got, metav1.DryRunAll)
	}
}
