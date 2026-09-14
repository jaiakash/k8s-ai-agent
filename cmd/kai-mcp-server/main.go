// Command kai-mcp-server exposes a Kubernetes cluster to MCP hosts.
//
// It holds no model and no prompt-to-kubectl translation: the host supplies
// the model, this server supplies the cluster. Run it over stdio from a local
// host (Claude Code, Goose, Cursor), or over Streamable HTTP behind an
// agentgateway for a shared deployment.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/jaiakash/k8s-ai-agent/internal/config"
	"github.com/jaiakash/k8s-ai-agent/internal/k8s"
	"github.com/jaiakash/k8s-ai-agent/internal/policy"
	"github.com/jaiakash/k8s-ai-agent/internal/tools"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

// instructions are sent to the host at initialize. They describe how to use
// this server's tools well; they are not a persona and not output formatting.
const instructions = `KAI exposes a Kubernetes cluster as MCP tools.

Investigating: list_pods reports an "issue" for every unhealthy pod and sorts
those first, so start there. get_pod adds container states, the previous
termination reason and the pod's own events. For a crash loop, read
get_pod_logs with previous=true, since the current container's logs usually
post-date the failure. list_events with warnings_only=true explains scheduling
and probe failures that pod status alone does not.

Changing things: every mutating tool defaults to dry_run=true, which asks the
API server to run admission, validation, quota checks and webhooks and then
discard the result. Call once with the default, report what would happen, and
only call again with dry_run=false once a human has agreed. Tools are
annotated with readOnlyHint and destructiveHint; respect them.

Permissions: this server may be read-only, or scoped to certain namespaces.
Read the k8s://policy resource to see what it allows. Permission failures are
reported before anything is attempted, so a denial means nothing happened.

Prefer the specific tool over get_resource; get_resource is for kinds that have
no dedicated tool, including custom resources.`

func main() {
	if err := run(); err != nil {
		// The stdio transport owns stdout, so diagnostics go to stderr.
		fmt.Fprintf(os.Stderr, "kai-mcp-server: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "", "Path to config.yaml.")
		transport   = flag.String("transport", "", "MCP transport: stdio (default) or http.")
		addr        = flag.String("addr", "", "Listen address for the http transport.")
		kubeconfig  = flag.String("kubeconfig", "", "Path to kubeconfig. Defaults to in-cluster credentials, then $KUBECONFIG, then ~/.kube/config.")
		kubeContext = flag.String("context", "", "Kubeconfig context to use.")
		allowWrites = flag.Bool("allow-writes", false, "Register mutating tools. Off by default: KAI is read-only unless asked otherwise.")
		dryRunOnly  = flag.Bool("dry-run-only", false, "Force every write to be a server-side dry run.")
		logLevel    = flag.String("log-level", "", "Log level: debug, info, warn or error.")
		showVersion = flag.Bool("version", false, "Print the version and exit.")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("kai-mcp-server %s\n", version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	// Flags win over the file and the environment, because they are the most
	// immediate statement of intent. Only flags the user actually set are
	// applied, so an unset flag's zero value never silently overrides config.
	applyFlagOverrides(&cfg, map[string]any{
		"transport":    transport,
		"addr":         addr,
		"kubeconfig":   kubeconfig,
		"context":      kubeContext,
		"allow-writes": allowWrites,
		"dry-run-only": dryRunOnly,
		"log-level":    logLevel,
	})

	if err := cfg.Validate(); err != nil {
		return err
	}

	log := newLogger(cfg.Server.LogLevel)

	client, err := k8s.New(k8s.Options{
		Kubeconfig: cfg.Kubernetes.Kubeconfig,
		Context:    cfg.Kubernetes.Context,
		QPS:        cfg.Kubernetes.QPS,
		Burst:      cfg.Kubernetes.Burst,
	})
	if err != nil {
		return fmt.Errorf("connect to cluster: %w", err)
	}

	// Fail at startup rather than on the first tool call. An MCP host that
	// connects successfully should be able to trust that the tools work.
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelProbe()
	info, err := client.ClusterInfo(probeCtx)
	if err != nil {
		return fmt.Errorf("reach cluster %q: %w", client.ContextName, err)
	}

	engine := policy.New(cfg.Policy)
	registry := tools.New(client, engine, log)

	mcpServer := server.NewMCPServer(
		"kai",
		version,
		server.WithInstructions(instructions),
		server.WithToolCapabilities(false),
		server.WithResourceCapabilities(false, false),
		server.WithPromptCapabilities(false),
		// Reject malformed tool arguments at the protocol edge, so handlers
		// never see input that does not match their declared schema.
		server.WithInputSchemaValidation(),
		server.WithRecovery(),
		server.WithLogger(log),
	)
	registry.Register(mcpServer)

	log.Info("connected",
		"context", info.Context,
		"serverVersion", info.ServerVersion,
		"nodes", info.Nodes,
		"metricsApi", info.MetricsAPI)
	log.Info("policy", "settings", cfg.Describe(), "tools", strings.Join(registry.ToolNames(), ","))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch cfg.Server.Transport {
	case config.TransportHTTP:
		return serveHTTP(ctx, mcpServer, cfg, log)
	default:
		log.Info("serving on stdio")
		return server.ServeStdio(mcpServer)
	}
}

// serveHTTP runs the Streamable HTTP transport, the current MCP remote
// transport. The deprecated HTTP+SSE transport is deliberately not offered.
func serveHTTP(ctx context.Context, mcpServer *server.MCPServer, cfg config.Config, log *slog.Logger) error {
	opts := []server.StreamableHTTPOption{
		server.WithEndpointPath("/mcp"),
		server.WithStreamableHTTPLogger(log),
		server.WithHeartbeatInterval(30 * time.Second),
	}
	if cfg.Server.Stateless {
		opts = append(opts, server.WithStateLess(true))
	}
	httpServer := server.NewStreamableHTTPServer(mcpServer, opts...)

	errCh := make(chan error, 1)
	go func() {
		log.Info("serving streamable http", "addr", cfg.Server.Addr, "path", "/mcp", "stateless", cfg.Server.Stateless)
		if err := httpServer.Start(cfg.Server.Addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		// Give in-flight tool calls a chance to finish; a killed kubectl-
		// equivalent mid-write is exactly what we are trying to avoid.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

// applyFlagOverrides copies flags the user actually set onto cfg.
//
// flag.Visit reports only flags present on the command line, which is what
// distinguishes "--allow-writes was not passed" from "--allow-writes=false".
func applyFlagOverrides(cfg *config.Config, flags map[string]any) {
	flag.Visit(func(f *flag.Flag) {
		value, ok := flags[f.Name]
		if !ok {
			return
		}
		switch f.Name {
		case "transport":
			cfg.Server.Transport = *value.(*string)
		case "addr":
			cfg.Server.Addr = *value.(*string)
		case "kubeconfig":
			cfg.Kubernetes.Kubeconfig = *value.(*string)
		case "context":
			cfg.Kubernetes.Context = *value.(*string)
		case "allow-writes":
			cfg.Policy.AllowWrites = *value.(*bool)
		case "dry-run-only":
			cfg.Policy.DryRunOnly = *value.(*bool)
		case "log-level":
			cfg.Server.LogLevel = *value.(*string)
		}
	})
}

// newLogger writes structured logs to stderr. Stdout belongs to the stdio
// transport: a stray log line there corrupts the JSON-RPC stream.
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
