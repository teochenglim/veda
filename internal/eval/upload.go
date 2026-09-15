package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// ErrPaymentRequired mirrors the paid-tier gate: cross-install benchmark
// upload is the paid piece; local scoring stays free.
var ErrPaymentRequired = fmt.Errorf("cross-install benchmarks are a paid feature — check your plan or token")

// UploadAggregate posts an anonymized benchmark report: scenario names and
// scores only — never memory content, never fixture text.
func UploadAggregate(ctx context.Context, endpoint, token string, version string, reports []*Report) error {
	type result struct {
		Scenario string  `json:"scenario"`
		Score    float64 `json:"score"`
		Passed   bool    `json:"passed"`
		Cases    int     `json:"cases"`
	}
	payload := struct {
		Version string   `json:"version"`
		Results []result `json:"results"`
	}{Version: version}
	for _, r := range reports {
		payload.Results = append(payload.Results, result{
			Scenario: r.Name, Score: r.Score, Passed: r.Passed, Cases: len(r.Cases),
		})
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		trimRight(endpoint, "/")+"/benchmarks", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusPaymentRequired {
		return ErrPaymentRequired
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("benchmark upload returned %d", resp.StatusCode)
	}
	return nil
}

func trimRight(s, cut string) string {
	for len(s) >= len(cut) && s[len(s)-len(cut):] == cut {
		s = s[:len(s)-len(cut)]
	}
	return s
}
