package config

// These fields were introduced together without rewriting older snapshots.
func backendSetting(key string) bool {
	return key == "reviewer.backend" || key == "feedback.backend" || key == "feedback.executable"
}

// Derive only built-in defaults, after all operator layers. An explicit model,
// effort or executable is never silently replaced when switching backend.
func (s *Snapshot) backendDefaults() {
	for _, prefix := range []string{"reviewer.", "feedback."} {
		backend := s.String(prefix + "backend")
		if backend == "claude" {
			for _, key := range []string{"model", "reasoning_effort"} {
				if s.origins[prefix+key].Layer == "built-in" {
					s.values[prefix+key] = ""
				}
			}
			if s.origins[prefix+"executable"].Layer == "built-in" {
				s.values[prefix+"executable"] = "claude"
			}
		}
	}
	if s.origins["feedback.executable"].Layer == "built-in" && s.String("feedback.backend") == "codex" {
		s.values["feedback.executable"] = "codex"
		// Existing Codex-only operators selected the shared executable through
		// reviewer.executable. Keep that meaning unless the lane opts out.
		if s.String("reviewer.backend") == "codex" {
			s.values["feedback.executable"] = s.String("reviewer.executable")
			s.origins["feedback.executable"] = s.origins["reviewer.executable"]
		}
	}
}
