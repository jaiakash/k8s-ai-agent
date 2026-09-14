package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestDefaultIsReadOnlyStdio(t *testing.T) {
	cfg := Default()
	if cfg.Server.Transport != TransportStdio {
		t.Errorf("default transport = %q, want stdio", cfg.Server.Transport)
	}
	if cfg.Policy.AllowWrites {
		t.Error("default policy must be read-only")
	}
	if !cfg.Policy.EnforceRBAC {
		t.Error("RBAC pre-flight should be on by default")
	}
}

func TestLoadWithNoPathUsesDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Transport != TransportStdio {
		t.Errorf("transport = %q, want stdio", cfg.Server.Transport)
	}
}

func TestLoadMissingExplicitPathIsAnError(t *testing.T) {
	// A typo'd --config must fail loudly rather than silently use defaults.
	if _, err := Load("/nonexistent/kai.yaml"); err == nil {
		t.Fatal("want an error for a missing explicit config path")
	}
}

func TestPartialConfigKeepsDefaults(t *testing.T) {
	path := writeConfig(t, `
policy:
  allowWrites: true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Policy.AllowWrites {
		t.Error("allowWrites from the file should be applied")
	}
	// Keys the file did not mention must keep their defaults, not go to zero.
	if cfg.Policy.MaxLogLines != Default().Policy.MaxLogLines {
		t.Errorf("MaxLogLines = %d, want the default %d", cfg.Policy.MaxLogLines, Default().Policy.MaxLogLines)
	}
	if len(cfg.Policy.ProtectedNamespaces) == 0 {
		t.Error("protectedNamespaces should keep its default when unspecified")
	}
	if cfg.Server.Transport != TransportStdio {
		t.Errorf("transport = %q, want the default stdio", cfg.Server.Transport)
	}
}

func TestFullConfigRoundTrip(t *testing.T) {
	path := writeConfig(t, `
server:
  transport: http
  addr: ":9090"
  logLevel: debug
kubernetes:
  context: staging
  qps: 50
policy:
  allowWrites: true
  dryRunOnly: true
  allowedNamespaces: [team-a, team-b]
  maxLogLines: 1000
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.Transport != TransportHTTP || cfg.Server.Addr != ":9090" {
		t.Errorf("server = %+v", cfg.Server)
	}
	if cfg.Kubernetes.Context != "staging" || cfg.Kubernetes.QPS != 50 {
		t.Errorf("kubernetes = %+v", cfg.Kubernetes)
	}
	if !cfg.Policy.DryRunOnly || len(cfg.Policy.AllowedNamespaces) != 2 {
		t.Errorf("policy = %+v", cfg.Policy)
	}
	if cfg.Policy.MaxLogLines != 1000 {
		t.Errorf("MaxLogLines = %d, want 1000", cfg.Policy.MaxLogLines)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	path := writeConfig(t, `
server:
  transport: stdio
policy:
  allowWrites: false
`)
	t.Setenv("KAI_TRANSPORT", "http")
	t.Setenv("KAI_ALLOW_WRITES", "true")
	t.Setenv("KAI_ALLOWED_NAMESPACES", "prod, staging")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Transport != TransportHTTP {
		t.Errorf("env should override file transport, got %q", cfg.Server.Transport)
	}
	if !cfg.Policy.AllowWrites {
		t.Error("env should override file allowWrites")
	}
	want := []string{"prod", "staging"}
	if len(cfg.Policy.AllowedNamespaces) != 2 ||
		cfg.Policy.AllowedNamespaces[0] != want[0] ||
		cfg.Policy.AllowedNamespaces[1] != want[1] {
		t.Errorf("AllowedNamespaces = %v, want %v (whitespace trimmed)", cfg.Policy.AllowedNamespaces, want)
	}
}

func TestDeprecatedSSETransportIsRejectedWithGuidance(t *testing.T) {
	cfg := Default()
	cfg.Server.Transport = "sse"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("the deprecated SSE transport must be rejected")
	}
	if !strings.Contains(err.Error(), "http") {
		t.Errorf("the error should point at the replacement transport, got: %v", err)
	}
}

func TestUnknownTransportRejected(t *testing.T) {
	cfg := Default()
	cfg.Server.Transport = "carrier-pigeon"
	if err := cfg.Validate(); err == nil {
		t.Fatal("want an error for an unknown transport")
	}
}

func TestContradictoryNamespaceListsRejected(t *testing.T) {
	cfg := Default()
	cfg.Policy.AllowedNamespaces = []string{"prod"}
	cfg.Policy.DeniedNamespaces = []string{"PROD"}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a namespace in both lists must be rejected")
	}
	if !strings.Contains(err.Error(), "prod") {
		t.Errorf("error should name the namespace, got: %v", err)
	}
}

func TestDescribeReportsMode(t *testing.T) {
	cfg := Default()
	if got := cfg.Describe(); !strings.Contains(got, "read-only") {
		t.Errorf("Describe() = %q, want it to report read-only", got)
	}

	cfg.Policy.AllowWrites = true
	if got := cfg.Describe(); !strings.Contains(got, "read-write") {
		t.Errorf("Describe() = %q, want read-write", got)
	}

	cfg.Policy.DryRunOnly = true
	if got := cfg.Describe(); !strings.Contains(got, "dry-run only") {
		t.Errorf("Describe() = %q, want the dry-run-only mode called out", got)
	}
}

func TestDescribeOmitsCredentials(t *testing.T) {
	cfg := Default()
	cfg.Kubernetes.Kubeconfig = "/home/user/.kube/config"

	// Describe goes to logs, which are frequently shipped somewhere else.
	if strings.Contains(cfg.Describe(), "/home/user") {
		t.Error("Describe() must not leak filesystem paths to credentials")
	}
}

func TestInvalidLogLevelRejected(t *testing.T) {
	cfg := Default()
	cfg.Server.LogLevel = "verbose"
	if err := cfg.Validate(); err == nil {
		t.Fatal("want an error for an unknown log level")
	}
}
