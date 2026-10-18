# Claude Code compatibility

Select `reviewer.backend = "claude"` and/or `feedback.backend = "claude"` in
operator configuration, or use `--backend claude` for a manual action. Install
and authenticate Claude Code normally; Codex is not required for Claude-only
operation. [Configuration](CONFIGURATION.md#native-agent-selection) covers mixed
lanes, project overrides, executables and model/effort defaults. Claude lanes
default to [Opus 5.5](https://platform.claude.com/docs/en/models/opus-5-5/overview)
(`claude-opus-5-5`), with native effort. Explicit model overrides, including
`model = ""` to inherit Claude's native model setting, remain supported.

## Native contract

The adapter is developed against installed **Claude Code 2.1.292**. It invokes:

```text
claude --print --output-format stream-json --verbose [--model MODEL] [--effort EFFORT] "/skill-name PR-URL"
```

Additional operator instructions follow the URL in the same argument. Every
launch is fresh: no `--continue`, `--resume`, `--from-pr` or fixed session ID.
Normal environment, authentication, settings, installed skills, plugins, MCP
servers and tool permissions remain inherited. Reviewd adds no `--bare`, tool
allow/deny flags, permission mode, prompt approval handler or sandbox. Native
headless mode has its own trust behavior: use an operator-trusted working
directory and configure native permissions normally. See the official
[headless contract](https://code.claude.com/docs/en/headless),
[CLI reference](https://code.claude.com/docs/en/cli-reference), and
[skills](https://code.claude.com/docs/en/skills).

A `system/init` establishes the session identity. Main-conversation `assistant`
messages provide text or payload-free tool/reasoning activity. Nested messages
cannot complete the main run. Completion requires exit zero and a matching
native `result` with `subtype: success`, `is_error: false`, and result text.
Error results, malformed/oversized streams, missing results/identity and failed
processes do not become completed runs. Native usage is retained without
inventing cross-provider token/cost equivalence; the result records Claude's
resolved model when reported. Existing `thread_id` JSON fields carry the native
session ID, while `executor = "claude_exec_v1"` identifies Claude unambiguously.
Historical `codex_exec_v1` records retain their Codex meaning.

Doctor checks only selected executable availability and version; it does not
validate authentication, model access, installed skills, MCP connectivity or
skill-owned effects. Reviewd does not resume or replay an uncertain execution.
Inspect its recorded native session before explicitly starting another action.

## Native integration evidence

The opt-in `TestNativeClaudeInheritsSkillsAndMCP` uses the installed CLI with
disposable operator-style settings, both fixture skills, a harmless project MCP
server and a synthetic local provider. Fixture trust/permission settings apply
only to its owned temporary project; production launch flags do not copy them.
It verifies skill expansion, actual MCP receipts, model/effort,
live tool activity and distinct fresh sessions. It does not certify personal authentication, paid-model quality, arbitrary
plugins, customer PR effects or native process-tree/background termination.
