// Package conflict implements v0.3's contradiction detection: cheap,
// explainable heuristics — no LLM call on the write path.
//
// Two rules produce a verdict:
//  1. Slot change: both memories bind the same life slot (residence,
//     employer, tool, role) to different values — "lives in Singapore" vs
//     "moved to Tokyo".
//  2. Negation: the incoming memory says "no longer / not anymore" and
//     shares enough significant tokens with the existing one.
//
// The detector fails open: anything it cannot confidently pair is left as
// -is (both memories stay active). False positives cost one click in the
// UI's Conflicts tab; false negatives cost nothing — both memories simply
// coexist as before v0.3.
package conflict

import (
	"regexp"
	"strings"
)

// Slot binds a life area to a value, e.g. {residence, Singapore}.
type Slot struct {
	Name  string
	Value string
}

// Analysis is the extractable structure of one memory.
type Analysis struct {
	Slots    []Slot
	Negation bool
}

// slotPatterns extract (slot, value) pairs. First match wins per pattern.
var slotPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"residence", regexp.MustCompile(`(?i)\b(?:lives? in|living in|is from|based in|moved to|relocated to|stays? in)\s+([a-z][a-z\s,.\-]{1,60})`)},
	{"employer", regexp.MustCompile(`(?i)\b(?:works? at|works? for|joined|hired by|quit|left)\s+([a-z][a-z0-9\s,.\-&]{1,60})`)},
	{"tool", regexp.MustCompile(`(?i)\b(?:uses?|using|switched to|migrated to)\s+([a-z][a-z0-9\s,.\-_]{1,60})`)},
	{"role", regexp.MustCompile(`(?i)\b(?:works? as|now an?|promoted to)\s+an?\s+([a-z][a-z\s\-]{2,60})`)},
}

var negationPattern = regexp.MustCompile(`(?i)\bno longer\b|\bnot anymore\b|\bno more\b|\bused to\b`)

// valueCleanup trims captured values at clause boundaries and punctuation.
var valueCleanup = regexp.MustCompile(`(?i)\s+\b(?:because|since|but|and|after|before|when|while|so|that|for\s+(?:work|school))\b.*$`)

// valueQualifiers cut trailing qualifiers so "Singapore near the coast" and
// "Singapore with their family" both normalize to "singapore".
var valueQualifiers = regexp.MustCompile(`(?i)\s+(?:near|with|at|on|by|as|since|during|inside|around|using)\b.*$`)
var valueTimePhrases = regexp.MustCompile(`(?i)\s+(?:(?:last|this|next)\s+(?:month|week|year|monday|tuesday|wednesday|thursday|friday|saturday|sunday)|(?:yesterday|today|recently|now)|\b\w+\s+ago)$`)
var trailingPunct = regexp.MustCompile(`[\s.,;:!]+$`)

// Analyze extracts slots and negation from one memory text.
func Analyze(text string) Analysis {
	var a Analysis
	for _, sp := range slotPatterns {
		if m := sp.re.FindStringSubmatch(text); m != nil {
			a.Slots = append(a.Slots, Slot{Name: sp.name, Value: normalizeValue(m[1])})
		}
	}
	a.Negation = negationPattern.MatchString(text)
	return a
}

// Verdict is the comparison result for a (existing, incoming) pair.
type Verdict struct {
	Conflict bool
	Reason   string
}

// Detect decides whether incoming supersedes existing.
func Detect(existing, incoming string) Verdict {
	ea, ia := Analyze(existing), Analyze(incoming)

	for _, is := range ia.Slots {
		for _, es := range ea.Slots {
			if is.Name == es.Name && is.Value != "" && es.Value != "" && is.Value != es.Value {
				return Verdict{
					Conflict: true,
					Reason:   is.Name + " changed: \"" + es.Value + "\" → \"" + is.Value + "\"",
				}
			}
		}
	}
	if ia.Negation && sharedSignificant(existing, incoming) >= 2 {
		return Verdict{Conflict: true, Reason: "negation of an existing memory"}
	}
	return Verdict{}
}

func normalizeValue(v string) string {
	v = valueCleanup.ReplaceAllString(v, "")
	v = valueQualifiers.ReplaceAllString(v, "")
	// trailing temporal phrases are timestamps, not part of the value
	for prev := ""; prev != v; {
		prev = v
		v = valueTimePhrases.ReplaceAllString(v, "")
	}
	v = trailingPunct.ReplaceAllString(v, "")
	return strings.ToLower(strings.Join(strings.Fields(v), " "))
}

// --- negation-rule token helpers ------------------------------------------

var stopwords = map[string]bool{
	"user": true, "they": true, "their": true, "them": true, "with": true,
	"that": true, "this": true, "from": true, "have": true, "has": true,
	"was": true, "were": true, "because": true, "when": true, "what": true,
	"some": true, "does": true, "also": true, "very": true, "really": true,
	"longer": true, "anymore": true, "more": true, "used": true,
}

func significantTokens(text string) map[string]bool {
	out := map[string]bool{}
	for _, tok := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z')
	}) {
		if len(tok) >= 4 && !stopwords[tok] {
			out[tok] = true
		}
	}
	return out
}

func sharedSignificant(a, b string) int {
	sa := significantTokens(a)
	n := 0
	for tok := range significantTokens(b) {
		if sa[tok] {
			n++
		}
	}
	return n
}
