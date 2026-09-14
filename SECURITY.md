# Security Policy

## Reporting a vulnerability

Report privately through
[GitHub Security Advisories](https://github.com/jaiakash/k8s-ai-agent/security/advisories/new).
Please do not open a public issue for a vulnerability.

Include the version or commit, how KAI was configured (policy settings and
whether writes were enabled), and what an attacker gets. A proof of concept
against a throwaway cluster helps more than anything else.

Expect an acknowledgement within 72 hours and an assessment within a week.

## What counts as a vulnerability

KAI's security model is that **an agent cannot do more than the operator
allowed**, in three independent layers. A way past any of them is a
vulnerability:

- **Policy bypass** — a mutating tool acting while `allowWrites` is false, any
  write to a `protectedNamespaces` entry, any access to a `deniedNamespaces`
  entry, an action outside `allowedNamespaces` while that list is set, or a
  write that commits while `dryRunOnly` is set.
- **Guard bypass** — a tool or resource that reaches the cluster without going
  through `Registry.guard` / `Registry.check`, and so skips policy or the
  `SelfSubjectAccessReview` pre-flight.
- **Privilege escalation** — anything letting KAI act beyond its
  ServiceAccount, or a Helm chart input that grants RBAC the policy did not ask
  for.
- **Injection** — tool input that escapes its parameter and changes the shape
  of the Kubernetes request (the resource, namespace, or verb actually called).
- **Leaking cluster data** past the policy that should have hidden it — for
  example a denied namespace's contents surfacing through `get_resource`,
  `list_events` or a `k8s://` resource.

Prompt injection reaching the *model* through cluster data is in scope only
where KAI could reasonably have prevented it. KAI assumes the model is
untrusted: that is the reason it never builds shell commands, and the reason
write tools are unregistered rather than refused. A model being persuaded to
call a tool the operator enabled is the host's problem, not KAI's — but a model
being able to call one the operator *disabled* is ours.

## What does not count

- **Cluster RBAC that is too broad.** If the ServiceAccount can delete
  everything, KAI will let an authorized caller delete things. Scope the
  ServiceAccount; the Helm chart derives least-privilege RBAC from your policy.
- **No authentication on the server.** This is by design — KAI enforces what
  may be asked, not who is asking. Put it behind an
  [agentgateway](deploy/agentgateway/README.md) and bind KAI to loopback.
- **Reading a Secret you granted read access to.** `get_resource` returns what
  RBAC permits. Deny Secrets at the RBAC layer, or list their namespace in
  `deniedNamespaces`.
- **Findings against a cluster you are not authorized to test.**

## Operating KAI safely

- Leave `allowWrites` off unless an agent genuinely needs to change things, and
  prefer `dryRunOnly` when a human is available to apply the change.
- Give KAI its own ServiceAccount. Do not reuse an admin kubeconfig.
- Keep `enforceRBAC` on. It is what turns an opaque 403 into a refusal that
  names the missing permission before anything is attempted.
- Treat `get_pod_logs` as more sensitive than the rest of the read surface;
  logs routinely contain tokens and customer data.
- Read `k8s://policy` from a running instance to confirm what it actually
  allows, rather than what its config file was meant to say.

## Supported versions

KAI is pre-1.0. Fixes land on `main` and in the next tagged release; there are
no backports to earlier tags yet.
