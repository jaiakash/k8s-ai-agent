// Package k8s wraps client-go for the tools KAI exposes over MCP.
//
// Every operation goes through the typed or dynamic client rather than
// shelling out to kubectl. That buys three things the exec approach cannot:
// RBAC pre-flight via SelfSubjectAccessReview, real server-side dry run, and
// typed objects that serialize straight into MCP structured output.
package k8s

import (
	"context"
	"fmt"

	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/jaiakash/k8s-ai-agent/internal/policy"
)

// Options configures how the client connects to a cluster.
type Options struct {
	// Kubeconfig is an explicit path. Empty uses in-cluster credentials when
	// present, otherwise the standard KUBECONFIG / ~/.kube/config resolution.
	Kubeconfig string
	// Context selects a kubeconfig context. Empty uses the current context.
	Context string
	QPS     float32
	Burst   int
}

// Client bundles the client-go surfaces the tools need.
//
// The fields are interfaces so tests can substitute client-go's fake
// implementations; see read_test.go.
type Client struct {
	Typed     kubernetes.Interface
	Dynamic   dynamic.Interface
	Metrics   metricsv.Interface
	Mapper    meta.RESTMapper
	Discovery discovery.DiscoveryInterface

	// ContextName and Host describe the connection, for cluster_info.
	ContextName string
	Host        string
}

// New builds a Client from opts, preferring in-cluster credentials.
func New(opts Options) (*Client, error) {
	restCfg, contextName, err := restConfig(opts)
	if err != nil {
		return nil, err
	}
	if opts.QPS > 0 {
		restCfg.QPS = opts.QPS
	}
	if opts.Burst > 0 {
		restCfg.Burst = opts.Burst
	}
	// Identify ourselves in audit logs. An operator reading the API server
	// audit trail should be able to tell KAI's calls from a human's kubectl.
	restCfg.UserAgent = "kai-mcp-server"

	typed, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("build typed client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("build dynamic client: %w", err)
	}
	// The metrics API is optional: metrics-server may not be installed. A
	// failure here is reported per-call by the top_* tools, not at startup.
	metricsClient, err := metricsv.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("build metrics client: %w", err)
	}

	// The shortcut expander resolves kubectl's short names ("deploy", "po",
	// "svc") using the cluster's own discovery data, so tool callers can use
	// the abbreviations they already know.
	disco := memory.NewMemCacheClient(typed.Discovery())
	var mapper meta.RESTMapper = restmapper.NewDeferredDiscoveryRESTMapper(disco)
	mapper = restmapper.NewShortcutExpander(mapper, disco, func(string) {})

	return &Client{
		Typed:       typed,
		Dynamic:     dyn,
		Metrics:     metricsClient,
		Mapper:      mapper,
		Discovery:   disco,
		ContextName: contextName,
		Host:        restCfg.Host,
	}, nil
}

// restConfig resolves credentials, preferring in-cluster.
func restConfig(opts Options) (*rest.Config, string, error) {
	if opts.Kubeconfig == "" && opts.Context == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, "in-cluster", nil
		}
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if opts.Kubeconfig != "" {
		rules.ExplicitPath = opts.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if opts.Context != "" {
		overrides.CurrentContext = opts.Context
	}

	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	cfg, err := loader.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("load kubeconfig: %w", err)
	}

	contextName := opts.Context
	if contextName == "" {
		if raw, err := loader.RawConfig(); err == nil {
			contextName = raw.CurrentContext
		}
	}
	return cfg, contextName, nil
}

// AccessError reports that the caller's credentials are insufficient. It is
// returned before the real request is made, so nothing has happened yet.
type AccessError struct {
	Action policy.Action
	Reason string
}

func (e *AccessError) Error() string {
	msg := fmt.Sprintf("RBAC denies %s for the credentials this server is using", e.Action)
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	return msg
}

// Can pre-flights an action with a SelfSubjectAccessReview.
//
// This is the difference between the agent saying "you do not have permission
// to delete deployments in prod" and the agent running a command that returns
// an opaque 403 after the fact. The review is evaluated by the API server
// against the same credentials the real call would use.
func (c *Client) Can(ctx context.Context, a policy.Action) error {
	review := &authzv1.SelfSubjectAccessReview{
		Spec: authzv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authzv1.ResourceAttributes{
				Namespace:   a.Namespace,
				Verb:        string(a.Verb),
				Group:       a.Group,
				Resource:    a.Resource,
				Subresource: a.Subresource,
				Name:        a.Name,
			},
		},
	}

	result, err := c.Typed.AuthorizationV1().SelfSubjectAccessReviews().
		Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("access review for %s failed: %w", a, err)
	}
	if !result.Status.Allowed || result.Status.Denied {
		return &AccessError{Action: a, Reason: result.Status.Reason}
	}
	return nil
}

// ResolveResource maps a user-supplied kind or resource name ("deploy",
// "Deployment", "deployments.apps") onto a concrete GroupVersionResource,
// including CRDs registered in the cluster.
func (c *Client) ResolveResource(arg string) (schema.GroupVersionResource, bool, error) {
	// ParseResourceArg splits "deployments.v1.apps" into a fully qualified
	// GVR; two-segment forms like "deployments.apps" come back as a
	// GroupResource that the mapper resolves to the preferred version.
	fullySpecified, groupResource := schema.ParseResourceArg(arg)
	input := groupResource.WithVersion("")
	if fullySpecified != nil {
		input = *fullySpecified
	}

	gvr, err := c.Mapper.ResourceFor(input)
	if err != nil {
		return schema.GroupVersionResource{}, false, fmt.Errorf(
			"unknown resource %q: %w (try the plural form, e.g. \"deployments\" or \"deployments.apps\")", arg, err)
	}

	// Namespaced-ness decides whether the caller must supply a namespace.
	// If the kind cannot be mapped we assume namespaced, which is the safer
	// default: a stray namespace on a cluster-scoped read is ignored, while a
	// missing one on a namespaced read would silently span the cluster.
	gvk, err := c.Mapper.KindFor(gvr)
	if err != nil {
		return gvr, true, nil
	}
	mapping, err := c.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return gvr, true, nil
	}
	return mapping.Resource, mapping.Scope.Name() == meta.RESTScopeNameNamespace, nil
}
