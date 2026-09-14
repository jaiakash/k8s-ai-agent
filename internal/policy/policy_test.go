package policy

import (
	"errors"
	"strings"
	"testing"
)

func TestDefaultIsReadOnly(t *testing.T) {
	e := New(Default())

	if err := e.Check(Action{Verb: VerbList, Resource: "pods", Namespace: "default"}); err != nil {
		t.Fatalf("reads must be allowed by default, got %v", err)
	}

	err := e.Check(Action{Verb: VerbDelete, Resource: "pods", Namespace: "default", Name: "web"})
	if err == nil {
		t.Fatal("writes must be denied by default")
	}
	var denied *DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("want *DeniedError, got %T", err)
	}
	if !strings.Contains(err.Error(), "allowWrites") {
		t.Errorf("denial should name the setting that unblocks it, got: %v", err)
	}
}

func TestProtectedNamespacesRejectWritesButAllowReads(t *testing.T) {
	cfg := Default()
	cfg.AllowWrites = true
	e := New(cfg)

	if err := e.Check(Action{Verb: VerbGet, Resource: "pods", Namespace: "kube-system"}); err != nil {
		t.Errorf("reads of a protected namespace should be allowed, got %v", err)
	}

	err := e.Check(Action{Verb: VerbDelete, Resource: "pods", Namespace: "kube-system", Name: "coredns"})
	if err == nil {
		t.Fatal("writes to a protected namespace must be denied")
	}
	if !strings.Contains(err.Error(), "protected") {
		t.Errorf("denial should explain protection, got: %v", err)
	}
}

func TestDeniedNamespacesHideReadsToo(t *testing.T) {
	cfg := Default()
	cfg.AllowWrites = true
	cfg.DeniedNamespaces = []string{"secrets-vault"}
	e := New(cfg)

	if err := e.Check(Action{Verb: VerbList, Resource: "pods", Namespace: "secrets-vault"}); err == nil {
		t.Fatal("denied namespaces must reject reads as well as writes")
	}
	if err := e.Check(Action{Verb: VerbList, Resource: "pods", Namespace: "default"}); err != nil {
		t.Errorf("unrelated namespace should be unaffected, got %v", err)
	}
}

func TestAllowedNamespacesIsAnAllowlist(t *testing.T) {
	cfg := Default()
	cfg.AllowedNamespaces = []string{"team-a", "team-b"}
	e := New(cfg)

	if err := e.Check(Action{Verb: VerbList, Resource: "pods", Namespace: "team-a"}); err != nil {
		t.Errorf("listed namespace should be allowed, got %v", err)
	}
	if err := e.Check(Action{Verb: VerbList, Resource: "pods", Namespace: "team-c"}); err == nil {
		t.Error("unlisted namespace must be denied")
	}
	// Cluster-scoped reads would leak names from outside the allowlist.
	if err := e.Check(Action{Verb: VerbList, Resource: "nodes"}); err == nil {
		t.Error("cluster-scoped actions must be denied while an allowlist is set")
	}
}

func TestNamespaceMatchingIsCaseInsensitive(t *testing.T) {
	cfg := Default()
	cfg.DeniedNamespaces = []string{"Prod"}
	e := New(cfg)

	if err := e.Check(Action{Verb: VerbList, Resource: "pods", Namespace: "prod"}); err == nil {
		t.Error("namespace matching should be case-insensitive")
	}
}

func TestWritesAllowedInOrdinaryNamespace(t *testing.T) {
	cfg := Default()
	cfg.AllowWrites = true
	e := New(cfg)

	if err := e.Check(Action{Verb: VerbPatch, Resource: "deployments", Group: "apps", Namespace: "default", Name: "web"}); err != nil {
		t.Fatalf("write should be permitted once allowWrites is set, got %v", err)
	}
}

func TestVerbClassification(t *testing.T) {
	for _, v := range []Verb{VerbGet, VerbList, VerbWatch} {
		if IsWrite(v) {
			t.Errorf("%s must not be a write verb", v)
		}
	}
	for _, v := range []Verb{VerbCreate, VerbUpdate, VerbPatch, VerbDelete} {
		if !IsWrite(v) {
			t.Errorf("%s must be a write verb", v)
		}
	}
	if IsDestructive(VerbCreate) {
		t.Error("create adds resources; it should not be flagged destructive")
	}
	if !IsDestructive(VerbDelete) {
		t.Error("delete must be flagged destructive")
	}
}

func TestMaxLogLinesFallsBackToDefault(t *testing.T) {
	e := New(Config{MaxLogLines: 0})
	if e.MaxLogLines() != Default().MaxLogLines {
		t.Errorf("unset MaxLogLines should fall back to %d, got %d", Default().MaxLogLines, e.MaxLogLines())
	}
}

func TestActionStringIsHumanReadable(t *testing.T) {
	got := Action{Verb: VerbGet, Resource: "pods", Subresource: "log", Namespace: "default", Name: "web"}.String()
	for _, want := range []string{"get", "pods/log", "default", "web"} {
		if !strings.Contains(got, want) {
			t.Errorf("Action.String() = %q, missing %q", got, want)
		}
	}
}
