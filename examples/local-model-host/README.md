# Driving KAI with a local model

A small MCP host in one file: an OpenAI-compatible endpoint on one side,
`kai-mcp-server` over stdio on the other.

It exists to make one claim concrete. KAI holds no model, so the model can be
anything — including one running on your own hardware, with no cluster data
leaving the machine. Nothing in this directory is KAI-specific: there is no
client library, no agent framework, and no prompt beyond the instructions the
server itself supplies at `initialize`.

For real use, run KAI under [Claude Code](https://claude.com/claude-code),
[Goose](https://block.github.io/goose/) or Cursor. This is a demonstration, not
a client — see [Not planned](../../ROADMAP.md) in the roadmap.

## Running it

Any OpenAI-compatible server with tool calling works. With
[vLLM](https://docs.vllm.ai):

```bash
vllm serve --model Qwen/Qwen3-14B-AWQ \
  --enable-auto-tool-choice --tool-call-parser hermes
```

Tool calling must be enabled — without `--enable-auto-tool-choice` the model
will describe the tool call in prose instead of emitting one.

```bash
go install github.com/jaiakash/k8s-ai-agent/cmd/kai-mcp-server@latest

python3 host.py "Why is the checkout service restarting?"
```

Only the standard library is needed. Anything after the question is passed
through to `kai-mcp-server`, so the usual flags apply:

```bash
python3 host.py "Scale web to 3 replicas" --allow-writes --dry-run-only
python3 host.py "What is unhealthy in staging?" --context staging
```

| Variable | Default | |
|---|---|---|
| `KAI_LLM` | `http://127.0.0.1:9000/v1` | OpenAI-compatible base URL |
| `KAI_MODEL` | `Qwen/Qwen3-14B-AWQ` | model name |
| `KAI_KEY` | — | API key, if your endpoint wants one |
| `KAI_SERVER` | found on `PATH` | path to the `kai-mcp-server` binary |

## What the loop actually does

1. `initialize`, then read the server's `instructions` and use them as the
   system prompt. The guidance for using these tools well ships with the
   tools, so every host gets it without copying a prompt around.
2. `tools/list`, and rename each entry into the OpenAI tool format. MCP input
   schemas are already JSON Schema, so this is a rename, not a translation.
3. Loop: ask the model, run any tool calls via `tools/call`, feed the results
   back, stop when it answers in prose.

A policy or RBAC refusal comes back as a normal tool result with `isError`,
not as a transport failure — so it goes back to the model, which explains it
to the human rather than crashing the host. The screenshots in the top-level
[README](../../README.md#what-it-looks-like) are this script.
