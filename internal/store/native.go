package store

// Native executors share scheduling and lifecycle authority; historical broker
// executors never acquire that authority through a backend default.
func IsNativeExecutor(executor string) bool {
	return executor == NativeExecutor || executor == ClaudeExecutor
}

func (r Run) Backend() string {
	if r.Executor == ClaudeExecutor {
		return "claude"
	}
	if r.Executor == NativeExecutor {
		return "codex"
	}
	return ""
}
