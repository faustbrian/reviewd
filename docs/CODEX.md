# Codex compatibility

The integration is tested with **Codex CLI 0.163.0** on macOS. It uses ordinary
fresh `codex exec` sessions, not the app-server API or an embedded model client.
The native fixture exercises installed skills, model/reasoning selection, JSONL
events and a harmless local MCP server with a synthetic local provider. This
does not certify a particular paid provider/model, every plugin, customer PR
effects, or native process-tree termination. Other Codex versions must supply
the same capabilities; no untested minimum-version claim is implied.

Required capabilities:

- `exec --json`, `--model`, `-c model_reasoning_effort="high"` (or the selected
  effort), and `--skip-git-repo-check` for URL-based work outside a checkout.
- JSONL `thread.started` with a stable session ID and `turn.completed` for a
  successful turn. `turn.failed`, malformed/incomplete streams or process
  failure cannot become successful execution.
- Native `item.started`, `item.updated` and `item.completed` envelopes. Reviewd
  displays a latest activity preview for known item kinds and agent messages;
  unknown events are tolerated without inventing completion.

The preview is at most 2 KiB, removes control characters, redacts recognized
GitHub/OpenAI credential formats before truncation, and omits command lines,
tool arguments/results and raw MCP payloads. It is a factual observation, not a
transcript, engineering approval or proof of delivery. `show`, JSON output and
TUI details expose it while the run is active. Final agent output is separate.

Reviewd inherits the normal environment, Codex configuration/authentication,
skills, plugins, permissions, sandbox and MCP/tools. It does not replace them,
restrict tool use, copy credentials or impose a workflow. The selected skill
and extra instructions own review, feedback handling and external effects.
Codex owns provider reconnection/retry behavior; reviewd records diagnostics
without independently replaying a started skill.

`doctor` reports executable/version availability, not model access, successful
authentication, installed-skill correctness or MCP reachability. Unsupported
flags or event formats fail the run; inspect `show <run-id>` and the recorded
Codex session before deciding whether to start fresh. An interrupted/uncertain
run is never automatically resumed or replayed.
