// Package policy decides whether a requested Kubernetes action is permitted
// before the agent ever reaches the API server.
//
// This is deliberately independent of client-go: it encodes operator intent
// ("this deployment of KAI may not write", "kube-system is off limits"), not
// cluster authorization. Cluster authorization is enforced separately by the
// API server, and pre-flighted via SelfSubjectAccessReview in internal/k8s.
//
// Both checks run for every tool call. Policy narrows what KAI offers; RBAC
// decides what the caller's credentials may actually do.
package policy

import (
	"fmt"
	"slices"
	"strings"
)

// Verb is a Kubernetes API verb.
type Verb string

const (
	VerbGet    Verb = "get"
	VerbList   Verb = "list"
	VerbWatch  Verb = "watch"
	VerbCreate Verb = "create"
	VerbUpdate Verb = "update"
	VerbPatch  Verb = "patch"
	VerbDelete Verb = "delete"
)

// writeVerbs mutate cluster state.
var writeVerbs = []Verb{VerbCreate, VerbUpdate, VerbPatch, VerbDelete}

// destructiveVerbs may remove or disrupt a running workload. These are the
// verbs surfaced to MCP clients with destructiveHint=true.
var destructiveVerbs = []Verb{VerbDelete, VerbUpdate, VerbPatch}

// IsWrite reports whether v mutates cluster state.
func IsWrite(v Verb) bool { return slices.Contains(writeVerbs, v) }

// IsDestructive reports whether v may disrupt a running workload.
func IsDestructive(v Verb) bool { return slices.Contains(destructiveVerbs, v) }

// Action is a single Kubernetes operation KAI wants to perform.
type Action struct {
	Verb      Verb
	Group     string // API group, "" for core
	Resource  string // plural, e.g. "pods"
	Namespace string // empty for cluster-scoped
	Name      string // optional
	// Subresource, e.g. "log" or "scale".
	Subresource string
}

func (a Action) String() string {
	res := a.Resource
	if a.Group != "" {
		res = a.Resource + "." + a.Group
	}
	if a.Subresource != "" {
		res += "/" + a.Subresource
	}
	if a.Name != "" {
		res += "/" + a.Name
	}
	if a.Namespace != "" {
		return fmt.Sprintf("%s %s in namespace %q", a.Verb, res, a.Namespace)
	}
	return fmt.Sprintf("%s %s (cluster-scoped)", a.Verb, res)
}

// Config is the operator-supplied policy, normally loaded from config.yaml.
type Config struct {
	// AllowWrites is the master switch for every mutating tool. When false
	// (the default) KAI is a read-only agent and write tools are not even
	// registered with the MCP server.
	AllowWrites bool `json:"allowWrites"`

	// DryRunOnly forces every permitted write to execute as a server-side
	// dry run. Useful for "let the agent propose changes, humans apply them".
	DryRunOnly bool `json:"dryRunOnly"`

	// ProtectedNamespaces reject writes even when AllowWrites is true.
	// Reads are still permitted.
	ProtectedNamespaces []string `json:"protectedNamespaces"`

	// AllowedNamespaces, when non-empty, is an allowlist: every action must
	// target one of these namespaces. Cluster-scoped actions are rejected.
	AllowedNamespaces []string `json:"allowedNamespaces"`

	// DeniedNamespaces are invisible to KAI entirely, reads included.
	DeniedNamespaces []string `json:"deniedNamespaces"`

	// MaxLogLines caps get_pod_logs so a single tool call cannot exhaust the
	// model's context window.
	MaxLogLines int64 `json:"maxLogLines"`

	// EnforceRBAC pre-flights each action with a SelfSubjectAccessReview so
	// the agent reports "you lack permission" instead of a raw 403.
	EnforceRBAC bool `json:"enforceRBAC"`
}

// Default returns the safe default policy: read-only, nothing hidden, RBAC
// pre-flight on. An operator opts into writes explicitly.
func Default() Config {
	return Config{
		AllowWrites:         false,
		DryRunOnly:          false,
		ProtectedNamespaces: []string{"kube-system", "kube-public", "kube-node-lease"},
		AllowedNamespaces:   nil,
		DeniedNamespaces:    nil,
		MaxLogLines:         500,
		EnforceRBAC:         true,
	}
}

// Engine evaluates actions against a Config.
type Engine struct {
	cfg Config
}

// New returns an Engine for cfg, filling in unset limits.
func New(cfg Config) *Engine {
	if cfg.MaxLogLines <= 0 {
		cfg.MaxLogLines = Default().MaxLogLines
	}
	return &Engine{cfg: cfg}
}

// Config returns the policy this engine enforces.
func (e *Engine) Config() Config { return e.cfg }

// AllowWrites reports whether any mutating tool should be registered.
func (e *Engine) AllowWrites() bool { return e.cfg.AllowWrites }

// DryRunOnly reports whether permitted writes must be server-side dry runs.
func (e *Engine) DryRunOnly() bool { return e.cfg.DryRunOnly }

// MaxLogLines is the hard cap on log lines returned by a single tool call.
func (e *Engine) MaxLogLines() int64 { return e.cfg.MaxLogLines }

// EnforceRBAC reports whether actions should be pre-flighted against RBAC.
func (e *Engine) EnforceRBAC() bool { return e.cfg.EnforceRBAC }

// DeniedError explains why an action was rejected. The message is written for
// the model to relay to a human, so it states the rule, not just "denied".
type DeniedError struct {
	Action Action
	Reason string
}

func (e *DeniedError) Error() string {
	return fmt.Sprintf("policy denied %s: %s", e.Action, e.Reason)
}

// Check evaluates a against the policy. A nil return means the action is
// permitted by local policy; the caller must still satisfy cluster RBAC.
func (e *Engine) Check(a Action) error {
	deny := func(format string, args ...any) error {
		return &DeniedError{Action: a, Reason: fmt.Sprintf(format, args...)}
	}

	if a.Namespace != "" && matches(e.cfg.DeniedNamespaces, a.Namespace) {
		return deny("namespace %q is in deniedNamespaces", a.Namespace)
	}

	if len(e.cfg.AllowedNamespaces) > 0 {
		if a.Namespace == "" {
			return deny("cluster-scoped actions are disabled while allowedNamespaces is set (%s)",
				strings.Join(e.cfg.AllowedNamespaces, ", "))
		}
		if !matches(e.cfg.AllowedNamespaces, a.Namespace) {
			return deny("namespace %q is not in allowedNamespaces (%s)",
				a.Namespace, strings.Join(e.cfg.AllowedNamespaces, ", "))
		}
	}

	if !IsWrite(a.Verb) {
		return nil
	}

	if !e.cfg.AllowWrites {
		return deny("this KAI instance is read-only; set policy.allowWrites=true to enable mutating tools")
	}

	if a.Namespace != "" && matches(e.cfg.ProtectedNamespaces, a.Namespace) {
		return deny("namespace %q is protected; writes to it are refused", a.Namespace)
	}

	return nil
}

// matches reports whether name is in list. An entry of "*" matches everything.
func matches(list []string, name string) bool {
	for _, item := range list {
		if item == "*" || strings.EqualFold(item, name) {
			return true
		}
	}
	return false
}
