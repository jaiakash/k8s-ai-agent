package k8s

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/jaiakash/k8s-ai-agent/internal/policy"
)

func testClient(objs ...runtime.Object) (*Client, *fake.Clientset) {
	cs := fake.NewClientset(objs...)
	return &Client{Typed: cs}, cs
}

func pod(name, ns string, mutate func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "app", Image: "nginx:1.27"}},
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: true}},
		},
	}
	if mutate != nil {
		mutate(p)
	}
	return p
}

func TestPodIssueDetectsCrashLoop(t *testing.T) {
	p := pod("web", "default", func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:         "app",
			Ready:        false,
			RestartCount: 7,
			State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
			},
		}}
	})

	if got := podIssue(p); got != "CrashLoopBackOff" {
		t.Errorf("podIssue = %q, want CrashLoopBackOff", got)
	}
}

func TestPodIssueReportsSchedulerMessageForPendingPods(t *testing.T) {
	p := pod("web", "default", func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodPending
		p.Status.ContainerStatuses = nil
		p.Status.Conditions = []corev1.PodCondition{{
			Type:    corev1.PodScheduled,
			Status:  corev1.ConditionFalse,
			Reason:  "Unschedulable",
			Message: "0/3 nodes are available: insufficient cpu",
		}}
	})

	got := podIssue(p)
	if !strings.Contains(got, "insufficient cpu") {
		t.Errorf("podIssue should relay the scheduler's message, got %q", got)
	}
}

func TestPodIssueEmptyForHealthyPod(t *testing.T) {
	if got := podIssue(pod("web", "default", nil)); got != "" {
		t.Errorf("healthy pod should report no issue, got %q", got)
	}
}

func TestPodIssueIgnoresSucceededJobPods(t *testing.T) {
	p := pod("job", "default", func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodSucceeded
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "app",
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", ExitCode: 0},
			},
		}}
	})

	if got := podIssue(p); got != "" {
		t.Errorf("a completed job pod is not an issue, got %q", got)
	}
}

func TestListPodsSurfacesUnhealthyFirst(t *testing.T) {
	healthy := pod("healthy", "default", nil)
	broken := pod("broken", "default", func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  "app",
			Ready: false,
			State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"},
			},
		}}
	})

	// Register the healthy pod first so ordering cannot pass by accident.
	c, _ := testClient(healthy, broken)

	got, err := c.ListPods(context.Background(), "default", "", "", 0)
	if err != nil {
		t.Fatalf("ListPods: %v", err)
	}
	if got.Count != 2 {
		t.Fatalf("Count = %d, want 2", got.Count)
	}
	if got.Unhealthy != 1 {
		t.Errorf("Unhealthy = %d, want 1", got.Unhealthy)
	}
	if got.Pods[0].Name != "broken" {
		t.Errorf("unhealthy pod should sort first, got %q", got.Pods[0].Name)
	}
	if got.Pods[0].Issue != "ImagePullBackOff" {
		t.Errorf("Issue = %q, want ImagePullBackOff", got.Pods[0].Issue)
	}
}

func TestListPodsAcrossAllNamespaces(t *testing.T) {
	c, _ := testClient(pod("a", "team-a", nil), pod("b", "team-b", nil))

	got, err := c.ListPods(context.Background(), "", "", "", 0)
	if err != nil {
		t.Fatalf("ListPods: %v", err)
	}
	if got.Count != 2 {
		t.Errorf("Count = %d, want 2 across namespaces", got.Count)
	}
	if got.Namespace != "(all namespaces)" {
		t.Errorf("Namespace = %q, want the all-namespaces label", got.Namespace)
	}
}

func TestGetPodReportsPreviousTermination(t *testing.T) {
	p := pod("web", "default", func(p *corev1.Pod) {
		p.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc"}}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:         "app",
			Ready:        false,
			RestartCount: 3,
			State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
			},
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					Reason:     "OOMKilled",
					ExitCode:   137,
					FinishedAt: metav1.NewTime(time.Now().Add(-time.Minute)),
				},
			},
		}}
	})
	c, _ := testClient(p)

	got, err := c.GetPod(context.Background(), "default", "web")
	if err != nil {
		t.Fatalf("GetPod: %v", err)
	}
	if len(got.Containers) != 1 {
		t.Fatalf("want 1 container, got %d", len(got.Containers))
	}
	if !strings.Contains(got.Containers[0].LastTerminated, "OOMKilled") {
		t.Errorf("LastTerminated should name OOMKilled, got %q", got.Containers[0].LastTerminated)
	}
	if got.OwnerKind != "ReplicaSet" || got.OwnerName != "web-abc" {
		t.Errorf("owner = %s/%s, want ReplicaSet/web-abc", got.OwnerKind, got.OwnerName)
	}
}

func TestGetPodNotFoundIsReadable(t *testing.T) {
	c, _ := testClient()

	_, err := c.GetPod(context.Background(), "default", "ghost")
	if err == nil {
		t.Fatal("want an error for a missing pod")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should say not found, got %v", err)
	}
}

func TestListDeploymentsFlagsUnreadyReplicas(t *testing.T) {
	three := int32(3)
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &three,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx:1.27"}}},
			},
		},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas: 1,
			Conditions: []appsv1.DeploymentCondition{{
				Type:    appsv1.DeploymentAvailable,
				Status:  corev1.ConditionFalse,
				Reason:  "MinimumReplicasUnavailable",
				Message: "Deployment does not have minimum availability.",
			}},
		},
	}
	c, _ := testClient(d)

	got, err := c.ListDeployments(context.Background(), "default", "")
	if err != nil {
		t.Fatalf("ListDeployments: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 deployment, got %d", len(got))
	}
	if got[0].Ready != "1/3" {
		t.Errorf("Ready = %q, want 1/3", got[0].Ready)
	}
	if !strings.Contains(got[0].Issue, "MinimumReplicasUnavailable") {
		t.Errorf("Issue should carry the deployment condition, got %q", got[0].Issue)
	}
	if got[0].Images != "nginx:1.27" {
		t.Errorf("Images = %q, want nginx:1.27", got[0].Images)
	}
}

func TestListNodesReportsPressureAndCordon(t *testing.T) {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-1",
			Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
		},
		Spec: corev1.NodeSpec{Unschedulable: true},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.33.0"},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue},
			},
		},
	}
	c, _ := testClient(n)

	got, err := c.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if got[0].Status != "Ready" {
		t.Errorf("Status = %q, want Ready", got[0].Status)
	}
	if got[0].Schedulable {
		t.Error("a cordoned node must report Schedulable=false")
	}
	if len(got[0].Pressure) != 1 || got[0].Pressure[0] != "MemoryPressure" {
		t.Errorf("Pressure = %v, want [MemoryPressure]", got[0].Pressure)
	}
	if got[0].Roles != "control-plane" {
		t.Errorf("Roles = %q, want control-plane", got[0].Roles)
	}
}

func TestListEventsPutsWarningsFirst(t *testing.T) {
	normal := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "e1", Namespace: "default"},
		Type:           corev1.EventTypeNormal,
		Reason:         "Pulled",
		Message:        "pulled image",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web"},
		LastTimestamp:  metav1.NewTime(time.Now()),
	}
	warning := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "e2", Namespace: "default"},
		Type:           corev1.EventTypeWarning,
		Reason:         "BackOff",
		Message:        "back-off restarting failed container",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web"},
		LastTimestamp:  metav1.NewTime(time.Now().Add(-time.Hour)),
	}
	c, _ := testClient(normal, warning)

	got, err := c.ListEvents(context.Background(), "default", 0, false)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if got.Warnings != 1 {
		t.Errorf("Warnings = %d, want 1", got.Warnings)
	}
	// The warning is older, so this only passes if type outranks recency.
	if got.Events[0].Reason != "BackOff" {
		t.Errorf("warnings must sort ahead of normal events, got %q first", got.Events[0].Reason)
	}
}

func TestListEventsRespectsLimit(t *testing.T) {
	objs := make([]runtime.Object, 0, 5)
	for i := range 5 {
		objs = append(objs, &corev1.Event{
			ObjectMeta:    metav1.ObjectMeta{Name: string(rune('a' + i)), Namespace: "default"},
			Type:          corev1.EventTypeNormal,
			Reason:        "Pulled",
			LastTimestamp: metav1.NewTime(time.Now()),
		})
	}
	c, _ := testClient(objs...)

	got, err := c.ListEvents(context.Background(), "default", 2, false)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if got.Count != 2 {
		t.Errorf("Count = %d, want the requested limit of 2", got.Count)
	}
}

func TestCanAllowsWhenRBACPermits(t *testing.T) {
	c, cs := testClient()
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authzv1.SelfSubjectAccessReview{
			Status: authzv1.SubjectAccessReviewStatus{Allowed: true},
		}, nil
	})

	err := c.Can(context.Background(), policy.Action{Verb: policy.VerbList, Resource: "pods", Namespace: "default"})
	if err != nil {
		t.Fatalf("Can should allow, got %v", err)
	}
}

func TestCanDeniesWithReason(t *testing.T) {
	c, cs := testClient()
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authzv1.SelfSubjectAccessReview{
			Status: authzv1.SubjectAccessReviewStatus{
				Allowed: false,
				Reason:  "no RoleBinding grants delete on deployments",
			},
		}, nil
	})

	err := c.Can(context.Background(), policy.Action{
		Verb: policy.VerbDelete, Group: "apps", Resource: "deployments", Namespace: "prod", Name: "web",
	})
	if err == nil {
		t.Fatal("Can should deny")
	}

	var accessErr *AccessError
	if !asAccessError(err, &accessErr) {
		t.Fatalf("want *AccessError, got %T", err)
	}
	if !strings.Contains(err.Error(), "RoleBinding") {
		t.Errorf("denial should relay the API server's reason, got %v", err)
	}
	// The failure must name what was attempted, so the model can explain it.
	if !strings.Contains(err.Error(), "prod") {
		t.Errorf("denial should name the namespace, got %v", err)
	}
}

// asAccessError is errors.As specialized to avoid importing errors twice in
// this file's narrow use.
func asAccessError(err error, target **AccessError) bool {
	for err != nil {
		if ae, ok := err.(*AccessError); ok {
			*target = ae
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestAgeFormatting(t *testing.T) {
	cases := []struct {
		ago  time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{90 * time.Minute, "1h30m"},
		{50 * time.Hour, "2d2h"},
	}
	for _, tc := range cases {
		got := age(metav1.NewTime(time.Now().Add(-tc.ago)))
		if got != tc.want {
			t.Errorf("age(%s) = %q, want %q", tc.ago, got, tc.want)
		}
	}
	if got := age(metav1.Time{}); got != "unknown" {
		t.Errorf("zero timestamp should render as unknown, got %q", got)
	}
}
