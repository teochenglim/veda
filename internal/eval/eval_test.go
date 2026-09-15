package eval

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teochenglim/veda/internal/config"
	"github.com/teochenglim/veda/internal/store"
)

// AC1: scenario files load (single + suite); malformed ones fail with
// actionable errors naming the scenario and the problem.
func TestAC1_ScenarioLoading(t *testing.T) {
	single := `{
		"name": "residence-recall",
		"setup": {"memories": [{"content": "User lives in Tokyo"}]},
		"cases": [{"query": "where do they live", "expect_contains": ["Tokyo"]}]
	}`
	scenarios, err := Load([]byte(single))
	if err != nil {
		t.Fatal(err)
	}
	if len(scenarios) != 1 || scenarios[0].Name != "residence-recall" {
		t.Fatalf("single scenario load: %+v", scenarios)
	}

	suite := `{"scenarios": [` + single + `, ` + single + `]}`
	if scenarios, err = Load([]byte(suite)); err != nil || len(scenarios) != 2 {
		t.Fatalf("suite load: %d (%v)", len(scenarios), err)
	}

	for _, bad := range []struct {
		name, json, wantErr string
	}{
		{"broken json", `{"name": "x", "cases": [`, "invalid scenario JSON"},
		{"missing cases", `{"name": "no-cases", "setup": {}}`, "at least one case"},
		{"missing query", `{"name": "x", "cases": [{"expect_contains": ["a"]}]}`, "missing query"},
		{"no expectations", `{"name": "x", "cases": [{"query": "q"}]}`, "expect_contains or expect_not_contains"},
		{"empty suite", `{"scenarios": []}`, "declares no scenarios"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			_, err := Load([]byte(bad.json))
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), bad.wantErr) {
				t.Fatalf("error %q should mention %q", err, bad.wantErr)
			}
		})
	}
}

// AC2: seeding + recall scoring — pass and fail cases score correctly
// against a throwaway store.
func TestAC2_RunScoresRecallCases(t *testing.T) {
	sc := Scenario{
		Name: "recall-scoring",
		Setup: Setup{Memories: []SeedMemory{
			{Content: "User lives in Tokyo near the bay"},
			{Content: "User drinks espresso every morning"},
		}},
		Cases: []Case{
			{Name: "hit", Query: "lives Tokyo", ExpectContains: []string{"Tokyo"}},
			{Name: "miss", Query: "drinks espresso", ExpectContains: []string{"matcha"}},
			{Name: "exclusion", Query: "espresso", ExpectContains: []string{"espresso"}, ExpectNotContains: []string{"Tokyo"}},
		},
	}
	rep, err := Run(sc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Passed {
		t.Fatal("the miss case must fail the scenario")
	}
	if rep.Score != 2.0/3.0 {
		t.Fatalf("score = %v, want 2/3", rep.Score)
	}
	var miss *CaseResult
	for i := range rep.Cases {
		if rep.Cases[i].Name == "miss" {
			miss = &rep.Cases[i]
		}
	}
	if miss == nil || miss.Passed || len(miss.Failures) == 0 {
		t.Fatalf("miss case must fail with reasons: %+v", miss)
	}
	// throwaway store cleaned up: the temp dir is gone after Run
}

// AC3: interference detection — disruption that crowds out recall fails;
// benign disruption does not.
func TestAC3_InterferenceDetection(t *testing.T) {
	base := Setup{Memories: []SeedMemory{{Content: "User's favorite editor is Cursor"}}}
	guard := []Case{{Name: "editor", Query: "favorite editor", TopK: 1, ExpectContains: []string{"Cursor"}}}

	disruptive := Scenario{
		Name:  "interference-detected",
		Setup: base,
		Cases: []Case{{Name: "sanity", Query: "marathons", ExpectNotContains: []string{"nonexistent-thing"}}},
		Interference: &Interference{
			Disrupt: Setup{Memories: []SeedMemory{
				{Content: "User's favorite editor is Zed", Salience: 1.0},
				{Content: "User switched editors to Zed", Salience: 1.0},
				{Content: "User codes in Zed now", Salience: 1.0},
			}},
			Guard: guard,
		},
	}
	rep, err := Run(disruptive, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Interference == nil || rep.Interference.Passed {
		t.Fatalf("crowding-out disruption must fail interference: %+v", rep.Interference)
	}
	if rep.Interference.Drop <= 0 {
		t.Fatalf("drop must be positive, got %v", rep.Interference.Drop)
	}

	benign := disruptive
	benign.Name = "interference-clean"
	benign.Interference = &Interference{
		Disrupt: Setup{Memories: []SeedMemory{{Content: "User enjoys night photography"}}},
		Guard:   guard,
	}
	rep, err = Run(benign, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Interference.Passed {
		t.Fatalf("benign disruption must not fail: %+v", rep.Interference)
	}
}

// AC4: faithfulness scoring flags memories not attributable to source turns.
func TestAC4_FaithfulnessFlagsInventions(t *testing.T) {
	sc := Scenario{
		Name: "faithfulness",
		Setup: Setup{
			Turns: []Turn{
				{Role: "user", Content: "I have a daughter named Ada who loves dinosaurs"},
			},
			Memories: []SeedMemory{
				{Content: "User has a young daughter named Ada"},  // faithful
				{Content: "User enjoys skydiving over Patagonia"}, // invention
			},
		},
		Cases: []Case{{Name: "sanity", Query: "ada", ExpectContains: []string{"Ada"}}},
	}
	rep, err := Run(sc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Faithfulness == nil {
		t.Fatal("faithfulness must run when turns are present")
	}
	if rep.Faithfulness.Flagged != 1 {
		t.Fatalf("expected exactly 1 flagged memory: %+v", rep.Faithfulness)
	}
	if !strings.Contains(rep.Faithfulness.Items[1].Content, "skydiving") || !rep.Faithfulness.Items[1].Flagged {
		t.Fatalf("the invention must be the flagged one: %+v", rep.Faithfulness.Items)
	}
	if rep.Passed {
		t.Fatal("flagged faithfulness must fail the scenario")
	}
}

// AC5: running eval never mutates the user's real store.
func TestAC5_EvalIsIsolatedFromRealStore(t *testing.T) {
	// simulate the user's real install
	home := t.TempDir()
	t.Setenv("VEDA_HOME", home)
	s, err := store.Open(filepath.Join(home, "veda.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.Remember(&store.Memory{Content: "User's real memory that eval must not touch"}, "cursor")
	before, _ := s.List("", "", zeroT(), zeroT(), 0, true)
	s.Close()

	sc := Scenario{
		Name:  "isolation",
		Setup: Setup{Memories: []SeedMemory{{Content: "isolation canary that must never leak into the real store"}}},
		Cases: []Case{{Name: "hit", Query: "isolation canary", ExpectContains: []string{"canary"}}},
	}
	rep, err := Run(sc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Passed {
		t.Fatal("eval scenario should pass in its own store")
	}

	// the real store is untouched: same single memory, no canary
	s2, err := store.Open(filepath.Join(home, "veda.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	after, _ := s2.List("", "", zeroT(), zeroT(), 0, true)
	if len(after) != len(before) {
		t.Fatalf("real store changed: %d -> %d", len(before), len(after))
	}
	for _, m := range after {
		if strings.Contains(m.Content, "canary") {
			t.Fatal("eval fixture leaked into the real store")
		}
	}
	// and the config default still points at the real home
	if config.Home() != home {
		t.Fatalf("VEDA_HOME override lost: %q", config.Home())
	}
}

// AC6: the paid benchmark upload posts aggregates only and honors the 402 gate.
func TestAC6_UploadPaidGate(t *testing.T) {
	var gotBody, gotAuth string
	okSrv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotBody, gotAuth = "", ""
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		gotBody = string(buf)
		gotAuth = r.Header.Get("Authorization")
	}))
	defer okSrv.Close()

	reports := []*Report{{
		Name: "bench", Passed: true, Score: 0.8,
		Cases: []CaseResult{{Name: "c", Query: "q with secret memory content", Passed: true}},
	}}
	if err := UploadAggregate(context.Background(), okSrv.URL, "plan-token", "v0.6.0", reports); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer plan-token" {
		t.Fatalf("token not sent: %q", gotAuth)
	}
	if strings.Contains(gotBody, "secret memory content") {
		t.Fatalf("upload leaked fixture content: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"score":0.8`) {
		t.Fatalf("aggregate scores must be present: %s", gotBody)
	}

	paidSrv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		http.Error(rw, "upgrade", http.StatusPaymentRequired)
	}))
	defer paidSrv.Close()
	err := UploadAggregate(context.Background(), paidSrv.URL, "tok", "v0.6.0", reports)
	if !errors.Is(err, ErrPaymentRequired) {
		t.Fatalf("expected ErrPaymentRequired, got %v", err)
	}
}

func zeroT() time.Time { return time.Time{} }
