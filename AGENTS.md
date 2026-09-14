# AGENTS.md

Guidance for coding agents working on KAI (k8s-ai-agent).

## What this project is

KAI is an **MCP server that exposes a Kubernetes cluster as tools**. It is not
an AI application. There is no model, no prompt-to-kubectl translation, and no
LLM client anywhere in this repository, and none should be added.

The host (Claude Code, Goose, Cursor, an agentgateway-fronted service) brings
the model. KAI brings the cluster. Keep that boundary — it is why the server
works with any host and can be tested without one.

If a change would put an LLM call inside this server, it is the wrong change.

## Build and test

```bash
make build          # build ./bin/kai-mcp-server
make test           # go test ./... with race detection
make lint           # gofmt check + go vet
make check          # lint + test, what CI runs
make run            # run on stdio against your current kubeconfig
```

Everything runs offline. No test requires a cluster: the Kubernetes layer is
tested against `k8s.io/client-go/kubernetes/fake`, and the MCP layer is tested
by driving real JSON-RPC messages through an in-process server.

Go 1.24 or newer.

## Layout

```
cmd/kai-mcp-server/   entrypoint, flags, transport selection
internal/config/      config.yaml + KAI_* env + flags
internal/k8s/         client-go: reads, writes, metrics, RBAC pre-flight
internal/policy/      operator guardrails, no client-go dependency
internal/tools/       MCP tools, resources and prompts
deploy/helm/kai/      chart with a least-privilege ServiceAccount
deploy/agentgateway/  example gateway config, and where auth actually lives
examples/             demonstrations only; nothing here ships in the binary
docs/images/          README screenshots, regenerated from examples/
server.json           MCP registry metadata; CI validates it against the schema
```

`server.json`, `deploy/helm/kai/Chart.yaml` and the image tag all carry the
version, and CI fails if they disagree. Bump them together.

## Rules that matter

**Every tool call passes through `Registry.guard`.** It runs local policy and
then a `SelfSubjectAccessReview`, in that order. A new tool that skips the
guard is a security bug, not a style problem. Resources use `Registry.check`,
which does the same thing — resources are another way in, not an exception.

**Write tools default to `dry_run=true`.** A write tool without a `dry_run`
parameter defaulting to true will fail `TestWriteToolsDefaultToDryRun`.

**Tool annotations must match behaviour.** `readOnlyHint` and
`destructiveHint` are what an MCP host uses to decide whether to interrupt a
human. A read tool annotated as destructive is merely noisy; a destructive tool
annotated as read-only is dangerous. `TestToolAnnotationsMatchBehaviour`
enforces this.

**Never shell out to `kubectl`.** Use client-go. Shelling out loses RBAC
pre-flight, real server-side dry run, and typed results, and it re-introduces
the command-injection surface this design exists to remove.

**Results are read by a language model.** Keep them compact and put the useful
fact first — `list_pods` sorts unhealthy pods to the top because that is what
the caller is looking for. Errors should say what to do next, not just what
went wrong. Compare `wrap()` in `internal/k8s/read.go`.

**Tool descriptions are the model's only documentation.** They must say when to
reach for the tool, not restate its name. There is a test for the floor.

## Adding a tool

1. Add the operation to `internal/k8s/read.go` or `write.go`, returning a
   small JSON-tagged struct.
2. Register it in `internal/tools/read_tools.go` or `write_tools.go` with a
   description, annotations, and an output schema.
3. Call `r.guard(...)` with the exact verb, group, resource and namespace the
   operation will use. For a subresource (`pods/log`, `deployments/scale`),
   guard the subresource: it has its own RBAC rule.
4. Add the name to `readToolNames` or `writeToolNames` — several tests iterate
   those lists.
5. If it is a write, add the RBAC rule to
   `deploy/helm/kai/templates/rbac.yaml` under the writes conditional.

## Style

Match the surrounding code. Comments explain *why* a decision was made, not
what the line does; several existing comments record a non-obvious tradeoff
(the fake clientset's scale subresource, the log byte cap, `Force` on apply)
and are worth keeping accurate if you change that code.

Do not add dependencies without a reason that survives asking "can the
standard library or client-go do this".

## Commits

Conventional commits (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`,
`chore:`). Run `make check` before committing.
