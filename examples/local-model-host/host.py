#!/usr/bin/env python3
"""A minimal MCP host: any OpenAI-compatible model + kai-mcp-server.

This exists to make one claim concrete: KAI holds no model, so it works with
whatever you point at it -- including a model running on your own hardware.
The whole host is the loop at the bottom of this file. There is no framework,
no agent library and no KAI-specific client, because none is needed.

It is an example, not a product. For real use, run KAI under Claude Code,
Goose, Cursor or any other MCP host; see the README.

Usage:
    # against a local vLLM server
    vllm serve --model Qwen/Qwen3-14B-AWQ --enable-auto-tool-choice \
        --tool-call-parser hermes

    python3 host.py "Why is the checkout service restarting?"

    # writes are off unless you say otherwise; everything after the question
    # is passed straight to kai-mcp-server
    python3 host.py "Scale web to 3" --allow-writes

Environment:
    KAI_LLM     OpenAI-compatible base URL   (default http://127.0.0.1:9000/v1)
    KAI_MODEL   model name                   (default Qwen/Qwen3-14B-AWQ)
    KAI_KEY     API key, if your endpoint wants one
    KAI_SERVER  path to the kai-mcp-server binary (default: found on PATH)
"""

import json
import os
import queue
import shutil
import subprocess
import sys
import threading
import urllib.request

LLM = os.environ.get("KAI_LLM", "http://127.0.0.1:9000/v1")
MODEL = os.environ.get("KAI_MODEL", "Qwen/Qwen3-14B-AWQ")
KEY = os.environ.get("KAI_KEY", "")
SERVER = os.environ.get("KAI_SERVER") or shutil.which("kai-mcp-server")

B, D, G, Y, M, R = "\033[1m", "\033[2m", "\033[32m", "\033[33m", "\033[35m", "\033[0m"


class MCPStdio:
    """MCP over stdio: newline-delimited JSON-RPC on the child's stdin/stdout.

    stdout carries the protocol and nothing else, so the server's own
    diagnostics go to stderr; we keep them for the error path.
    """

    def __init__(self, argv):
        self.proc = subprocess.Popen(
            argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, text=True, bufsize=1,
        )
        self.replies = queue.Queue()
        self.stderr = []
        threading.Thread(target=self._pump_stdout, daemon=True).start()
        threading.Thread(target=self._pump_stderr, daemon=True).start()
        self.next_id = 0

    def _pump_stdout(self):
        for line in self.proc.stdout:
            if line.strip():
                self.replies.put(json.loads(line))

    def _pump_stderr(self):
        for line in self.proc.stderr:
            self.stderr.append(line.rstrip())

    def call(self, method, params=None, notify=False):
        msg = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            msg["params"] = params
        if not notify:
            self.next_id += 1
            msg["id"] = self.next_id
        try:
            self.proc.stdin.write(json.dumps(msg) + "\n")
            self.proc.stdin.flush()
        except BrokenPipeError:
            raise SystemExit("kai-mcp-server exited:\n  " + "\n  ".join(self.stderr[-10:]))
        return None if notify else self.replies.get(timeout=60)

    def close(self):
        self.proc.stdin.close()
        self.proc.wait(timeout=5)


def complete(messages, tools):
    """One chat completion against an OpenAI-compatible endpoint."""
    payload = json.dumps({
        "model": MODEL, "messages": messages, "tools": tools,
        "tool_choice": "auto", "temperature": 0.1, "max_tokens": 900,
    }).encode()
    headers = {"Content-Type": "application/json"}
    if KEY:
        headers["Authorization"] = f"Bearer {KEY}"
    req = urllib.request.Request(f"{LLM}/chat/completions", data=payload, headers=headers)
    with urllib.request.urlopen(req, timeout=300) as resp:
        return json.loads(resp.read())["choices"][0]["message"]


def main():
    if not SERVER:
        raise SystemExit("kai-mcp-server not found on PATH; set KAI_SERVER")
    if len(sys.argv) < 2:
        raise SystemExit(__doc__)
    question, server_args = sys.argv[1], sys.argv[2:]

    mcp = MCPStdio([SERVER, *server_args])
    init = mcp.call("initialize", {
        "protocolVersion": "2025-06-18",
        "capabilities": {},
        "clientInfo": {"name": "local-model-host", "version": "1"},
    })["result"]
    mcp.call("notifications/initialized", notify=True)

    # The server tells the host how to use it well. Passing this through as the
    # system prompt is the entire "prompt engineering" this host does -- the
    # guidance lives with the tools, so every host gets it.
    instructions = init.get("instructions", "")
    mcp_tools = mcp.call("tools/list", {})["result"]["tools"]

    print(f"{B}model{R}  {MODEL} {D}({LLM}){R}")
    print(f"{B}server{R} kai-mcp-server {D}{' '.join(server_args) or '(read-only)'}{R}")
    print(f"{B}tools{R}  {len(mcp_tools)} exposed\n")
    print(f"{B}user{R}   {question}\n")

    # MCP tool definitions are already JSON Schema, which is what the OpenAI
    # tool format wants, so this is a rename rather than a translation.
    tools = [{
        "type": "function",
        "function": {
            "name": t["name"],
            "description": t.get("description", ""),
            "parameters": t.get("inputSchema", {"type": "object", "properties": {}}),
        },
    } for t in mcp_tools]

    messages = [{"role": "system", "content": instructions},
                {"role": "user", "content": question}]

    for _ in range(8):
        reply = complete(messages, tools)
        calls = reply.get("tool_calls") or []
        if not calls:
            print(f"{B}{G}model{R}  {(reply.get('content') or '').strip()}")
            break

        messages.append({"role": "assistant", "content": reply.get("content") or "",
                         "tool_calls": calls})
        for call in calls:
            name = call["function"]["name"]
            args = json.loads(call["function"]["arguments"] or "{}")
            print(f"{Y}  -> {name}{R}({D}{json.dumps(args)}{R})")

            result = mcp.call("tools/call", {"name": name, "arguments": args}).get("result", {})
            text = "\n".join(c.get("text", "") for c in result.get("content", [])
                             if c.get("type") == "text")

            # A policy or RBAC refusal is a normal tool result, not a crash:
            # it goes back to the model, which explains it to the human.
            if result.get("isError"):
                print(f"{M}     refused{R} {text}")
            else:
                for line in text.strip().split("\n")[:6]:
                    print(f"{D}     {line}{R}")

            messages.append({"role": "tool", "tool_call_id": call["id"],
                             "name": name, "content": text[:4000]})
        print()

    mcp.close()


if __name__ == "__main__":
    main()
