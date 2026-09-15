package gate

import "testing"

// AC4: the cheap gate uses regex + length + role heuristics only — no LLM.
func TestAC4_CheapGate(t *testing.T) {
	cases := []struct {
		name      string
		role      string
		content   string
		candidate bool
	}{
		{"first-person preference", "user", "I prefer tea over coffee in the afternoon because caffeine keeps me up", true},
		{"remember directive", "user", "Remember that my daughter's name is Ada and she is six", true},
		{"assistant durable fact", "assistant", "The user mentioned they work as a backend engineer in Singapore", true},
		{"durable usage fact", "user", "I use Cursor for work and switched to it from VS Code last year", true},
		{"chit-chat rejected", "user", "ok thanks that works for me, see you later then", false},
		{"too short", "user", "I like it", false},
		{"too long paste", "user", string(make([]byte, 3000)) + "I prefer tea", false},
		{"secret rejected", "user", "my api_key is sk-1234567890abcdefghij1234 and remember that", false},
		{"password rejected", "user", "remember that my password is hunter2correcthorse", false},
		{"bad role", "system", "I prefer window seats on every flight I take", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Check(c.role, c.content)
			if got.Candidate != c.candidate {
				t.Fatalf("Check(%q) candidate=%v (want %v), reason=%q", c.content, got.Candidate, c.candidate, got.Reason)
			}
			if got.Candidate && (got.Score <= 0 || got.Score > 1) {
				t.Fatalf("candidate score out of range: %v", got.Score)
			}
		})
	}
}
