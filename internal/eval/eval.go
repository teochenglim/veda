// Package eval implements v0.6's advanced eval harness: fixture scenarios
// scored against a throwaway store — never the user's real ~/.veda.
//
// Three measurements per scenario:
//   - recall cases (query → expect_contains / expect_not_contains, top-k)
//   - interference (recall degradation after ingesting disruptor memories)
//   - faithfulness (token-attribution of distilled/seeded memories to the
//     fixture turns they came from — flags inventions)
//
// The paid tier (cross-install benchmarks) is the Upload hook: anonymized
// aggregate scores only, gated by the backend's 402.
package eval

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Suite wraps multiple scenarios in one file.
type Suite struct {
	Scenarios []Scenario `json:"scenarios"`
}

// Scenario is one fixture + expectation set.
type Scenario struct {
	Name         string        `json:"name"`
	Description  string        `json:"description,omitempty"`
	Setup        Setup         `json:"setup"`
	Cases        []Case        `json:"cases"`
	Interference *Interference `json:"interference,omitempty"`
}

// Setup seeds the throwaway store: pre-distilled memories and/or raw turns
// (turns distill through the gate + user LLM when one is configured).
type Setup struct {
	Memories []SeedMemory `json:"memories,omitempty"`
	Turns    []Turn       `json:"turns,omitempty"`
}

type SeedMemory struct {
	Content    string  `json:"content"`
	Type       string  `json:"type,omitempty"`
	Confidence float64 `json:"confidence,omitempty"` // default 1.0
	Salience   float64 `json:"salience,omitempty"`   // default 0.5
}

type Turn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Case is one recall expectation.
type Case struct {
	Name              string   `json:"name,omitempty"`
	Query             string   `json:"query"`
	TopK              int      `json:"top_k,omitempty"`          // default 5
	MinConfidence     float64  `json:"min_confidence,omitempty"` // default 0.5
	Semantic          bool     `json:"semantic,omitempty"`
	ExpectContains    []string `json:"expect_contains,omitempty"`
	ExpectNotContains []string `json:"expect_not_contains,omitempty"`
}

// Interference measures recall degradation after ingesting disruptor setup.
type Interference struct {
	Disrupt Setup  `json:"disrupt"`
	Guard   []Case `json:"guard"`
	// MaxDrop is the allowed pass-rate drop (0..1) after disruption.
	// Default 0: any degradation fails.
	MaxDrop float64 `json:"max_drop,omitempty"`
}

// Load parses a scenario file: either a single scenario object or a suite
// {"scenarios": [...]}. Malformed input fails with actionable errors.
func Load(data []byte) ([]Scenario, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("invalid scenario JSON at offset %d: %w", jsonOffset(err), err)
	}
	switch {
	case probe["scenarios"] != nil:
		var suite Suite
		if err := json.Unmarshal(data, &suite); err != nil {
			return nil, fmt.Errorf("invalid suite: %w", err)
		}
		if len(suite.Scenarios) == 0 {
			return nil, fmt.Errorf("suite declares no scenarios")
		}
		for i, sc := range suite.Scenarios {
			if err := validate(sc, i); err != nil {
				return nil, err
			}
		}
		return suite.Scenarios, nil
	default: // a single scenario object; validate() reports what's missing
		var sc Scenario
		if err := json.Unmarshal(data, &sc); err != nil {
			return nil, fmt.Errorf("invalid scenario: %w", err)
		}
		if err := validate(sc, 0); err != nil {
			return nil, err
		}
		return []Scenario{sc}, nil
	}
}

func validate(sc Scenario, i int) error {
	label := sc.Name
	if label == "" {
		label = fmt.Sprintf("#%d", i)
	}
	if strings.TrimSpace(sc.Name) == "" {
		return fmt.Errorf("scenario %s: missing name", label)
	}
	if len(sc.Cases) == 0 {
		return fmt.Errorf("scenario %q: at least one case is required", sc.Name)
	}
	for j, c := range sc.Cases {
		if strings.TrimSpace(c.Query) == "" {
			return fmt.Errorf("scenario %q case #%d: missing query", sc.Name, j)
		}
		if len(c.ExpectContains) == 0 && len(c.ExpectNotContains) == 0 {
			return fmt.Errorf("scenario %q case #%d: needs expect_contains or expect_not_contains", sc.Name, j)
		}
	}
	if sc.Interference != nil && len(sc.Interference.Guard) == 0 {
		return fmt.Errorf("scenario %q: interference requires guard cases", sc.Name)
	}
	return nil
}

func jsonOffset(err error) int {
	if je, ok := err.(*json.UnmarshalTypeError); ok && je.Offset > 0 {
		return int(je.Offset)
	}
	if se, ok := err.(*json.SyntaxError); ok {
		return int(se.Offset)
	}
	return 0
}

// FaithThreshold is the minimum token-attribution score for a memory to
// count as faithful to its source turns.
const FaithThreshold = 0.3
