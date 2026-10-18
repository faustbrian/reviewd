# Architecture and verification

```text
GitHub reads → discovery/event identity → SQLite inbox + operator policy
                                            ↓
                                  foreground scheduler
                                            ↓
                                  ordinary Codex exec
                                            ↓
                           native output/session/process outcome
```

`internal/discovery` and `internal/observation` identify work; `config` resolves
settings; `watch` admits it; `dispatch` owns the launch and durable process
projection; `agent` adapts Codex JSONL. `store`, `lease` and `lifetime` retain
process ownership, deduplication and stop behavior. `notify`, `cli` and `terminal`
are presentation/attention surfaces. Legacy schema and resource decoding exist
to preserve earlier state, not to resume its former workflow engine.

The scheduler captures the operator document at job pickup and passes it through
GitHub preparation and dispatch. Reload does not replace that selection. A busy
PR defers discovered work back to the same queue for a later pickup; a manual
URL invocation keeps its explicit selection while waiting. Durable event/hold
checks remain separate from configuration selection. The total selected timeout
starts before preparation, not again at native launch.

Codex receives a literal skill invocation and PR URL, selected model/effort and
optional instructions. No output schema, evidence proxy, custom specialist
protocol, copied authentication or app-owned external publisher is inserted.
The working directory is operator-selected or inherited. Native retries are
left to Codex; recoverable error events are diagnostics, not proof the turn
failed. A missing final completion is not success.

```sh
python3 scripts/check.py
python3 -B -m unittest discover -s scripts -p test_package.py -v
REVIEWD_NATIVE_AGENT=1 go test ./internal/agent -run TestNativeCodexInherits -count=1
```

The normal quality script uses disposable Go caches and builds the four
supported targets. The opt-in native check uses the installed Codex with a
private test-owned configuration, installed fixture skills, a local synthetic
provider and a harmless MCP server. It uses no real provider credentials or
customer PR effects. It tests normal skill/MCP/config inheritance, not model
quality, production credentials or customer workflow delivery.

Hosted quality and packaging checks are separate from local focused tests.
A build, test result or completed native fixture is not a release or deployment.
