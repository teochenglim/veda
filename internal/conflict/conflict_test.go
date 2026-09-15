package conflict

import "testing"

func TestDetect_SlotChange(t *testing.T) {
	cases := []struct {
		name     string
		existing string
		incoming string
		conflict bool
	}{
		{
			// AC1 canonical fixture: same residence slot, different value
			name:     "residence move",
			existing: "User lives in Singapore with their family",
			incoming: "User moved to Tokyo last month for work",
			conflict: true,
		},
		{
			name:     "employer change",
			existing: "User works at Acme Corp on the payments team",
			incoming: "User joined Globex as a staff engineer",
			conflict: true,
		},
		{
			name:     "tool switch",
			existing: "User uses Cursor for all their coding work",
			incoming: "User switched to Zed last week and loves it",
			conflict: true,
		},
		{
			name:     "same slot same value is not a conflict",
			existing: "User lives in Singapore near the coast",
			incoming: "User lives in Singapore with their family",
			conflict: false,
		},
		{
			// negation rule: explicit retraction + enough shared tokens
			name:     "negation retraction",
			existing: "User still drinks coffee every single morning",
			incoming: "User no longer drinks coffee in the morning",
			conflict: true,
		},
		{
			// related but compatible facts must coexist
			name:     "same topic different facts",
			existing: "User has a daughter named Ada",
			incoming: "User's daughter Ada is six years old",
			conflict: false,
		},
		{
			name:     "unrelated topics",
			existing: "User prefers window seats on flights",
			incoming: "User drinks espresso instead of drip coffee",
			conflict: false,
		},
		{
			name:     "chit-chat",
			existing: "The weather in Singapore is hot and humid",
			incoming: "It rains a lot in Singapore during December",
			conflict: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Detect(c.existing, c.incoming)
			if got.Conflict != c.conflict {
				t.Fatalf("Detect conflict=%v (want %v), reason=%q", got.Conflict, c.conflict, got.Reason)
			}
		})
	}
}

func TestAnalyze_SlotExtraction(t *testing.T) {
	a := Analyze("User moved to Tokyo last month for work")
	if len(a.Slots) != 1 || a.Slots[0].Name != "residence" || a.Slots[0].Value != "tokyo" {
		t.Fatalf("slots: %+v", a.Slots)
	}
	if a.Negation {
		t.Fatal("no negation expected")
	}
	a = Analyze("User no longer works at Acme Corp")
	if !a.Negation {
		t.Fatal("negation missed")
	}
	if len(a.Slots) == 0 || a.Slots[0].Name != "employer" {
		t.Fatalf("employer slot missed: %+v", a.Slots)
	}
	// value capture stops at clause boundaries
	a = Analyze("User lives in Singapore because rent is cheap")
	if a.Slots[0].Value != "singapore" {
		t.Fatalf("value capture should stop at clause boundary: %q", a.Slots[0].Value)
	}
}
