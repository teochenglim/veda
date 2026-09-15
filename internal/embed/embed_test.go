package embed

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// AC4: the client has exactly one network destination — the configured
// BaseURL — and sends only the batched input texts to it.
func TestAC4_SingleConfiguredDestination(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		json.NewEncoder(rw).Encode(map[string]any{
			"data": []any{
				map[string]any{"index": 0, "embedding": []float32{1, 0}},
				map[string]any{"index": 1, "embedding": []float32{0, 1}},
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "secret-key", "test-embed-model")
	vecs, err := c.Embed(context.Background(), []string{"first text", "second text"})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/embeddings" {
		t.Fatalf("expected /embeddings path, got %q", gotPath)
	}
	var req map[string]any
	json.Unmarshal([]byte(gotBody), &req)
	if req["model"] != "test-embed-model" {
		t.Fatalf("model not sent: %v", req)
	}
	if len(vecs) != 2 || vecs[0][0] != 1 || vecs[1][1] != 1 {
		t.Fatalf("vectors not returned in input order: %v", vecs)
	}
}

func TestClientErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		http.Error(rw, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := New(srv.URL, "", "m")
	if _, err := c.Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("expected error from failing endpoint")
	}
	// empty input is a no-op, no network call
	if v, err := c.Embed(context.Background(), nil); err != nil || v != nil {
		t.Fatalf("empty input: %v %v", v, err)
	}
}

func TestCosine(t *testing.T) {
	if got := Cosine([]float32{1, 0}, []float32{1, 0}); got < 0.99 {
		t.Fatalf("identical vectors: %v", got)
	}
	if got := Cosine([]float32{1, 0}, []float32{0, 1}); got != 0 {
		t.Fatalf("orthogonal vectors: %v", got)
	}
	if got := Cosine([]float32{1}, []float32{1, 0}); got != 0 {
		t.Fatalf("dim mismatch must be 0, got %v", got)
	}
	if got := Cosine(nil, nil); got != 0 {
		t.Fatalf("empty vectors: %v", got)
	}
}
