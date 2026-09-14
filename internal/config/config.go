// Package config loads KAI's settings from a YAML file, the environment, and
// command-line flags, in that order of increasing precedence.
//
// Three sources sounds like a lot for one small server, but each has a job:
// the file is what an operator commits and reviews, the environment is what a
// Helm chart and a container runtime can set, and flags are what a developer
// types. Nothing is read from anywhere else.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/jaiakash/k8s-ai-agent/internal/policy"
)

// Transport names an MCP transport.
const (
	// TransportStdio is the default. MCP hosts launch the server as a
	// subprocess and speak JSON-RPC over stdin/stdout.
	TransportStdio = "stdio"
	// TransportHTTP is Streamable HTTP, the current MCP remote transport.
	// The older HTTP+SSE transport is deprecated and is not implemented.
	TransportHTTP = "http"
)

// Server holds transport settings.
type Server struct {
	Transport string `json:"transport"`
	Addr      string `json:"addr"`
	// Stateless serves each HTTP request independently, with no session
	// affinity. Required when several replicas sit behind a load balancer.
	Stateless bool   `json:"stateless"`
	LogLevel  string `json:"logLevel"`
}

// Kubernetes holds cluster connection settings.
type Kubernetes struct {
	Kubeconfig string  `json:"kubeconfig"`
	Context    string  `json:"context"`
	QPS        float32 `json:"qps"`
	Burst      int     `json:"burst"`
}

// Config is the whole of KAI's configuration.
type Config struct {
	Server     Server        `json:"server"`
	Kubernetes Kubernetes    `json:"kubernetes"`
	Policy     policy.Config `json:"policy"`
}

// Default returns the configuration used when nothing is supplied: a
// read-only server on stdio, which is the safest useful thing KAI can be.
func Default() Config {
	return Config{
		Server: Server{
			Transport: TransportStdio,
			Addr:      ":8080",
			LogLevel:  "info",
		},
		Kubernetes: Kubernetes{
			QPS:   20,
			Burst: 40,
		},
		Policy: policy.Default(),
	}
}

// Load reads path (when non-empty), then applies environment overrides.
//
// A missing file is an error only when the path was given explicitly. That
// distinction matters: silently ignoring a typo'd --config would run the
// server with defaults an operator did not intend.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config %s: %w", path, err)
		}
		// Unmarshal onto the defaults, so an absent key keeps its default
		// rather than becoming a zero value.
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	applyEnv(&cfg)

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// applyEnv overlays KAI_* environment variables.
func applyEnv(cfg *Config) {
	str := func(key string, target *string) {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			*target = v
		}
	}
	boolean := func(key string, target *bool) {
		if v, ok := os.LookupEnv(key); ok {
			if b, err := strconv.ParseBool(v); err == nil {
				*target = b
			}
		}
	}
	list := func(key string, target *[]string) {
		if v, ok := os.LookupEnv(key); ok {
			*target = splitList(v)
		}
	}

	str("KAI_TRANSPORT", &cfg.Server.Transport)
	str("KAI_ADDR", &cfg.Server.Addr)
	str("KAI_LOG_LEVEL", &cfg.Server.LogLevel)
	boolean("KAI_STATELESS", &cfg.Server.Stateless)

	str("KAI_KUBECONFIG", &cfg.Kubernetes.Kubeconfig)
	str("KAI_CONTEXT", &cfg.Kubernetes.Context)

	boolean("KAI_ALLOW_WRITES", &cfg.Policy.AllowWrites)
	boolean("KAI_DRY_RUN_ONLY", &cfg.Policy.DryRunOnly)
	boolean("KAI_ENFORCE_RBAC", &cfg.Policy.EnforceRBAC)
	list("KAI_ALLOWED_NAMESPACES", &cfg.Policy.AllowedNamespaces)
	list("KAI_DENIED_NAMESPACES", &cfg.Policy.DeniedNamespaces)
	list("KAI_PROTECTED_NAMESPACES", &cfg.Policy.ProtectedNamespaces)

	if v, ok := os.LookupEnv("KAI_MAX_LOG_LINES"); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.Policy.MaxLogLines = n
		}
	}
}

// splitList parses a comma-separated list, dropping empty entries. An empty
// string yields an empty (non-nil) slice, so it can clear a configured list.
func splitList(v string) []string {
	out := []string{}
	for _, part := range strings.Split(v, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// Validate rejects settings that would fail confusingly later.
func (c *Config) Validate() error {
	switch c.Server.Transport {
	case TransportStdio, TransportHTTP:
	case "sse":
		return fmt.Errorf(
			"transport \"sse\" is the deprecated HTTP+SSE transport and is not supported; use %q, which is the current MCP remote transport",
			TransportHTTP)
	default:
		return fmt.Errorf("unknown transport %q: use %q or %q", c.Server.Transport, TransportStdio, TransportHTTP)
	}

	if c.Server.Transport == TransportHTTP && c.Server.Addr == "" {
		return fmt.Errorf("server.addr is required for the %s transport", TransportHTTP)
	}

	switch strings.ToLower(c.Server.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("unknown log level %q: use debug, info, warn or error", c.Server.LogLevel)
	}

	// A namespace in both lists is a contradiction the operator should fix
	// rather than have silently resolved one way or the other.
	for _, allowed := range c.Policy.AllowedNamespaces {
		for _, denied := range c.Policy.DeniedNamespaces {
			if strings.EqualFold(allowed, denied) {
				return fmt.Errorf("namespace %q is in both allowedNamespaces and deniedNamespaces", allowed)
			}
		}
	}
	return nil
}

// Describe renders the effective settings for startup logging. It never
// includes credentials; only the kubeconfig *path* is reported.
func (c *Config) Describe() string {
	mode := "read-only"
	if c.Policy.AllowWrites {
		mode = "read-write"
		if c.Policy.DryRunOnly {
			mode = "read-write (dry-run only)"
		}
	}

	parts := []string{
		"transport=" + c.Server.Transport,
		"mode=" + mode,
		"enforceRBAC=" + strconv.FormatBool(c.Policy.EnforceRBAC),
	}
	if len(c.Policy.AllowedNamespaces) > 0 {
		parts = append(parts, "allowedNamespaces="+strings.Join(c.Policy.AllowedNamespaces, ","))
	}
	if len(c.Policy.ProtectedNamespaces) > 0 {
		parts = append(parts, "protectedNamespaces="+strings.Join(c.Policy.ProtectedNamespaces, ","))
	}
	return strings.Join(parts, " ")
}
