# Roadmap

What KAI is, what it is next, and what it is deliberately not.

## Shipped

**Phase 1 — a real MCP server.** The LLM is out of the server; the host brings
the model. Twelve read-only tools on client-go, MCP resources and prompts,
stdio and Streamable HTTP transports, structured output with annotations.

**Phase 2 — safe writes.** Six mutating tools, all dry-run by default, behind a
policy engine and `SelfSubjectAccessReview` pre-flight. Helm chart whose RBAC
is derived from the configured policy, and an agentgateway config for shared
deployments.

## Next

### Cluster awareness

- `list_workloads` covering StatefulSets, DaemonSets, Jobs and CronJobs, not
  just Deployments.
- Ingress and NetworkPolicy tools — "why can't A reach B" is a top-three
  question and KAI currently cannot answer it.
- `explain_resource` backed by the OpenAPI schema, so the agent can check a
  field exists before proposing it.
- Resource templates (`k8s://namespace/{name}/health`) so a host can attach one
  namespace's state as context.

### Diagnosis

- Correlate across sources: a pod that is Pending, a node under memory
  pressure, and a Warning event are one finding, not three.
- Rollout history and `rollback_deployment`, so a bad deploy has an undo.
- Probe analysis — distinguishing "the app is broken" from "the readiness
  probe is wrong", which look identical in pod status.

### Operations

- Audit log of every tool call with its policy decision, as structured events.
  Today this goes to stderr; it should be queryable.
- Prometheus metrics for tool calls, denials and latency.
- `PodDisruptionBudget` awareness before a restart or cordon.

## Not planned

Being clear about this saves everyone time:

- **A built-in chat client, TUI or web UI.** Use an MCP host. Not maintaining a
  client is a feature, not a gap.
- **An LLM inside the server.** This was the original design and it was the
  main thing wrong with it. The host owns the model.
- **Shelling out to `kubectl`.** It loses RBAC pre-flight, real server-side dry
  run and typed results, and it re-introduces command injection with a language
  model on the untrusted end.
- **A policy DSL.** The policy engine covers read-only, namespace scoping and
  protected namespaces. Anything more expressive belongs in RBAC, in
  OPA/Gatekeeper/Kyverno at admission, or in agentgateway's CEL rules — all of
  which already exist and are better at it.
- **Write access to Secrets.** Reading them is already gated by RBAC and should
  usually be denied. KAI will not grow a tool that writes them.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Items under "Next" are good places to
start; open an issue before a large change so we can agree the shape first.
