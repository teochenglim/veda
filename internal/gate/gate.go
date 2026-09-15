// Package gate implements Veda's "cheap gate": regex + length + role
// heuristics that decide whether a turn is a memory candidate — no LLM call.
// It must over-trigger slightly rather than miss; the human reviews pending
// turns in the UI anyway.
package gate

import (
	"regexp"
	"strings"
)

// secretPattern matches content that looks like a credential; such turns are
// never promoted to candidates even if they contain first-person phrasing
// ("my password is ..." must not end up in a memory).
var secretPattern = regexp.MustCompile(`(?i)(api[_-]?key|secret|password|passwd|token|authorization:\s*bearer|BEGIN (RSA )?PRIVATE KEY)`)

// signalPatterns are first-person, durable-fact phrasings. Each match adds
// to the candidate's gate score.
var signalPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bi am\b|\bi'm\b|\bi was born\b`),
	regexp.MustCompile(`(?i)\bmy name is\b|\bcall me\b`),
	regexp.MustCompile(`(?i)\bi (?:prefer|like|love|hate|avoid|dislike)\b`),
	regexp.MustCompile(`(?i)\bremember (?:that|this)\b`),
	regexp.MustCompile(`(?i)\bi (?:work|live|study|was born)\b`),
	regexp.MustCompile(`(?i)\bmy (?:wife|husband|partner|son|daughter|dog|cat|birthday|timezone|goal|project|team|manager)\b`),
	regexp.MustCompile(`(?i)\bi (?:use|am using|switched to|migrated to)\b`),
	regexp.MustCompile(`(?i)\bthe user (?:mentioned|mentioned that|said|prefers|works|lives|is)\b`),
	regexp.MustCompile(`(?i)\balways\b|\bnever\b|\bevery (?:morning|day|week)\b`),
}

// MinLen / MaxLen bound candidate content: below MinLen there is nothing to
// distill, above MaxLen it is likely a document paste, not a fact.
const (
	MinLen = 16
	MaxLen = 2000
)

// Result is the gate verdict for one turn.
type Result struct {
	Candidate bool
	Score     float64 // 0..1 heuristic confidence
	Reason    string  // why it was rejected, for debugging
}

// Check applies the heuristics to one turn.
func Check(role, content string) Result {
	if role != "user" && role != "assistant" {
		return Result{Reason: "role not user/assistant"}
	}
	trimmed := strings.TrimSpace(content)
	n := len(trimmed)
	if n < MinLen {
		return Result{Reason: "too short"}
	}
	if n > MaxLen {
		return Result{Reason: "too long (likely a paste)"}
	}
	if secretPattern.MatchString(trimmed) {
		return Result{Reason: "looks like a secret"}
	}
	score := 0.0
	for _, p := range signalPatterns {
		if p.MatchString(trimmed) {
			score += 0.25
		}
	}
	if score == 0 {
		return Result{Reason: "no first-person signals"}
	}
	if score > 1 {
		score = 1
	}
	return Result{Candidate: true, Score: score}
}
