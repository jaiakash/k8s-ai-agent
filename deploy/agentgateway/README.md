# Authenticating KAI

KAI has no authentication of its own. It enforces *what* may be asked of a
cluster, not *who* is asking — and an MCP server that tries to solve both ends
up doing neither well. Identity is supplied by
[agentgateway](https://agentgateway.dev) in front of it.

```
MCP host ──▶ agentgateway ──▶ kai-mcp-server ──▶ Kubernetes
             JWT, per-tool     policy, RBAC
             authz, limits     pre-flight, dry run
```

Run KAI bound to loopback so the gateway is the only way in:

```bash
kai-mcp-server --transport http --addr 127.0.0.1:8080 --allow-writes
agentgateway -f deploy/agentgateway/config.yaml
```

Hosts then connect to the gateway, not to KAI.

## Why KAI does not serve `/.well-known/oauth-protected-resource`

The MCP authorization spec says an MCP server **MUST** implement
[RFC 9728](https://datatracker.ietf.org/doc/html/rfc9728) Protected Resource
Metadata — but that requirement is scoped:

> Authorization is **OPTIONAL** for MCP implementations. When supported:
> implementations using an HTTP-based transport **SHOULD** conform to this
> specification. Implementations using an STDIO transport **SHOULD NOT** follow
> this specification, and instead retrieve credentials from the environment.

KAI validates no tokens, so it is not an OAuth resource server and has no
authorization server to name. Serving the document anyway would advertise a
protection that does not exist: a client would read "present a token from this
issuer", and KAI would then serve any request that arrived without one. That is
worse than not publishing it, because it converts a visible gap into an
invisible one.

The metadata belongs to whatever terminates authentication — here, the gateway.
[`well-known/oauth-protected-resource.json`](well-known/oauth-protected-resource.json)
is that document; serve it from the gateway (or the proxy in front of it) at
exactly:

```
/.well-known/oauth-protected-resource
```

Edit `resource` to the canonical URI clients use (scheme and host, no fragment —
`https://kai.your-company.example.com/mcp`) and `authorization_servers` to your
issuer. Both must match the gateway's `jwtAuth` block in `config.yaml`, or
clients will discover an issuer whose tokens the gateway then rejects.

An unauthenticated request must be answered with a `401` that points at it:

```http
HTTP/1.1 401 Unauthorized
WWW-Authenticate: Bearer resource_metadata="https://kai.your-company.example.com/.well-known/oauth-protected-resource",
                         scope="kai:read"
```

and an authenticated request whose token lacks a scope with `403` and
`error="insufficient_scope"`, naming every scope that operation needs in one
challenge — clients re-authorize on the union, so drip-feeding scopes costs a
round trip each time.

**Stdio needs none of this.** When a host launches `kai-mcp-server` itself, the
spec says explicitly not to use this flow: credentials come from the
environment, which for KAI means the kubeconfig.

## Scopes

`scopes_supported` in the metadata is the minimal useful set:

| Scope | Covers |
|---|---|
| `kai:read` | the read-only tools, and the `k8s://` resources |
| `kai:logs` | `get_pod_logs` — separate because logs carry secrets, tokens and customer data |
| `kai:write` | the mutating tools, which `--allow-writes` must also have enabled |

The CEL rules in [`config.yaml`](config.yaml) key off `jwt.groups` rather than
scopes, because group membership is what most identity providers already carry.
To key off scopes instead, swap the group test for a scope test:

```yaml
- 'mcp.tool.name == "get_pod_logs" && "kai:logs" in jwt.scope.split(" ")'
```

Whichever you choose, the gateway's rules and the published `scopes_supported`
have to agree. Two layers describing different permissions is how a caller ends
up holding a token that discovery says is sufficient and the gateway says is not.

## What the gateway does not replace

Authorization at the gateway decides who may call a tool. It does not decide
what that tool may then do to the cluster — KAI's policy and the
ServiceAccount's RBAC still apply, and a caller who is authorized to call
`delete_pod` still cannot use it against a protected namespace. Keep both:
the gateway knows who is asking, KAI knows what is safe to do.
