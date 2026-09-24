# Firefly Concert

A concurrent HTTP server exercise: a concert's 64×64 light wall that phones send bursts of
light to and screens read back, under 10,000 requests per second.

**Start with [`PROMPT.md`](PROMPT.md).** It has the task, the API, the error rules, the AI
policy, and how you are scored.

| File | What it is |
|---|---|
| `PROMPT.md` | The task |
| `grader/bin/` | The grader, prebuilt for macOS, Linux and Windows. Same for every stack |
| `grader/` | The grader's source, if you want to read exactly what it checks |
| `example_snapshot.json` | Exact expected JSON for the worked example |
| `visualize_wall.py` | Renders a wall snapshot as a PNG (needs Python 3) |
| `AGENTS.md`, `CLAUDE.md`, `.claude/settings.json` | Rules for your AI coding assistant |
| `NOTES.md` | Your submission notes. Write these yourself |

Quick start, once your server is listening on port 8765:

```sh
grader/bin/firefly-grader-darwin-arm64 --url http://127.0.0.1:8765 --correctness-only
```

Pick the binary for your machine: `-darwin-arm64`, `-darwin-amd64`, `-linux-amd64`,
`-linux-arm64`, or `-windows-amd64.exe`.
