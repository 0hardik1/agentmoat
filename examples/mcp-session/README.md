# Worked agentmoat-mcp session

These files capture short, replayable JSON-RPC exchanges with
`./bin/agentmoat-mcp`. The line-oriented `.jsonl` files can be replayed
directly:

```bash
./bin/agentmoat-mcp --log-level=silent < 01-explain.req.jsonl
```

This produces an output stream that matches `01-explain.resp.jsonl`
(modulo `generatedAt` timestamps and the absolute path to your
kubeconfig). The captured responses are pretty-printed across multiple
lines for readability; the actual wire encoding is single-line per
message, as the stdio transport requires.

| File                                         | What it demonstrates                                                |
|----------------------------------------------|---------------------------------------------------------------------|
| `01-explain.req.jsonl`                       | initialize + `tools/call explain` (offline; no cluster needed).     |
| `01-explain.resp.jsonl`                      | Server's responses to the above.                                    |
| `02-scan-and-plan.req.jsonl`                 | initialize + `tools/call scan_cluster` + `tools/call propose_plan`. |
| `02-scan-and-plan.resp.jsonl`                | Server's responses (annotated; bodies abbreviated).                 |

Run `make mcp-smoke` for the automated equivalent of file 02 against
the kind cluster.
