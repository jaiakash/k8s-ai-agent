# Contributing to KAI

Thanks for your interest. KAI is small and deliberately narrow, so the fastest
route to a merged PR is understanding what the project is trying to be.

## What KAI is

An MCP server that exposes a Kubernetes cluster as tools. The host supplies the
model; KAI supplies the cluster.

This means some otherwise reasonable contributions are out of scope:

- **An LLM client, prompt template, or "ask the AI" tool.** KAI had one of
  these once and it was the main thing wrong with the design. The model lives
  in the host.
- **Shelling out to `kubectl`.** Use client-go. See below.
- **A bundled chat client or TUI.** Use Goose, Claude Code, Cursor, or any
  other MCP host. Not maintaining a client is a feature.

Everything else — new tools, better diagnostics, more resources and prompts,
deployment improvements — is welcome.

## Getting set up

You need Go 1.24+. A cluster is optional: no test needs one.

```bash
git clone https://github.com/jaiakash/k8s-ai-agent
cd k8s-ai-agent
make check      # lint + test, the same thing CI runs
```

To try it against a real cluster:

```bash
make run        # stdio, read-only, your current kubeconfig
```

Or point any MCP host at the built binary — see the README.

## Design rules

These are enforced by tests, and a PR that breaks one will fail CI.

**Every tool call goes through `Registry.guard`.** It runs local policy, then a
`SelfSubjectAccessReview` against the API server. Both, in that order, on every
call. MCP resources use `Registry.check`, which does the same — a resource is
another way in, not an exception to the rule.

**Write tools default to `dry_run=true`.** The agent's first attempt at any
change is a rehearsal. Committing takes a second, explicit call.

**Annotations must tell the truth.** `readOnlyHint` and `destructiveHint` are
what an MCP host uses to decide whether to interrupt a human before running
something. A destructive tool annotated read-only is a safety bug.

**Never `exec` anything.** Beyond losing RBAC pre-flight, real dry run and
typed results, shelling out re-introduces command injection: the arguments
would come from a language model.

**Results are read by a model, not a person.** Keep them compact, put the
important thing first, and make errors say what to do next.

## Adding a tool

1. Add the operation to `internal/k8s/`, returning a small JSON-tagged struct.
2. Register it in `internal/tools/read_tools.go` or `write_tools.go`, with a
   description that says *when to use it*, correct annotations, and an output
   schema.
3. Guard it with the exact verb, group, resource and namespace it will use.
   Subresources (`pods/log`, `deployments/scale`) have their own RBAC rules —
   guard the subresource.
4. Add the name to `readToolNames` or `writeToolNames`.
5. For a write, add the RBAC rule to `deploy/helm/kai/templates/rbac.yaml`
   under the `allowWrites` conditional.
6. Add tests. The Kubernetes layer uses client-go's fake clientset; the MCP
   layer is tested by sending real JSON-RPC through an in-process server
   (`internal/tools/protocol_test.go`).

## Pull requests

- One logical change per PR.
- Conventional commit subjects: `feat:`, `fix:`, `docs:`, `refactor:`,
  `test:`, `chore:`.
- Run `make check` first.
- Say what you tested against. "Read-only on a kind cluster" is useful;
  "should work" is not.

## Reporting a security issue

Do not open a public issue for a vulnerability. Report it privately through
GitHub's security advisory form on the repository.

KAI's threat model assumes the model calling it may be adversarial — prompt
injection through pod annotations, log contents or ConfigMap data is expected.
Anything that lets a tool call step outside the configured policy or the
ServiceAccount's RBAC is a vulnerability. Anything the ServiceAccount was
legitimately granted is not.

## Code of conduct

Be decent. Assume good faith, give specific feedback, and keep discussion on
the technical merits.
