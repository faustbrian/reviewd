// Package activity formats a latest-status preview, never a tool transcript.
package activity

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A status card and its durable record retain at most 2 KiB, independently of
// Codex's larger response budget. Full final output remains a separate outcome.
const Limit = 2048

var secrets = regexp.MustCompile(`sk-(?:proj-)?[A-Za-z0-9_-]{20,}|gh[opusr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{20,}`)

func Redact(text string) string { return secrets.ReplaceAllString(text, "<redacted>") }

func Clean(text string) string {
	// Redact before truncation so cutting a token cannot defeat its recognizer.
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, Redact(text))
	if len(text) > Limit {
		text = text[:Limit]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	return strings.TrimSpace(text)
}
