// Package llm is a minimal OpenAI-compatible chat-completions client used by
// the async worker to distill pending turns into memories. The user supplies
// the key and endpoint; Veda never ships one.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Client talks to any OpenAI-compatible /chat/completions endpoint.
type Client struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    *http.Client
}

func New(baseURL, apiKey, model string) *Client {
	return &Client{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   model,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

// Fact is one memory the model distilled from a batch of turns.
type Fact struct {
	Type       string  `json:"type"`
	Content    string  `json:"content"`
	Confidence float64 `json:"confidence"`
	Salience   float64 `json:"salience"`
}

const systemPrompt = `You distill a person's conversation turns into durable memories about them.
Return ONLY a JSON array. Each element: {"type":"preference|fact|identity|goal","content":"<one concise sentence, third-person about the user>","confidence":0.0-1.0,"salience":0.0-1.0}.
Rules: only durable facts or preferences (not chit-chat); merge duplicates; max 10 items; empty array [] if nothing qualifies.`

// Distill sends a batch of turns and parses the JSON array response.
func (c *Client) Distill(ctx context.Context, turns []string) ([]Fact, error) {
	if len(turns) == 0 {
		return nil, nil
	}
	body := "Conversation turns:\n"
	for i, t := range turns {
		body += fmt.Sprintf("%d. %s\n", i+1, t)
	}
	reqBody, err := json.Marshal(chatRequest{
		Model:       c.Model,
		Temperature: 0,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: body},
		},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		trimRight(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("llm status %d", resp.StatusCode)
	}
	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil, err
	}
	if len(cr.Choices) == 0 {
		return nil, fmt.Errorf("llm returned no choices")
	}
	return parseFacts(cr.Choices[0].Message.Content)
}

func parseFacts(s string) ([]Fact, error) {
	// Tolerate markdown fences around the JSON array.
	start := bytes.IndexByte([]byte(s), '[')
	end := bytes.LastIndexByte([]byte(s), ']')
	if start < 0 || end <= start {
		return nil, nil // model said nothing qualifies
	}
	var facts []Fact
	if err := json.Unmarshal([]byte(s[start:end+1]), &facts); err != nil {
		return nil, fmt.Errorf("parse facts: %w", err)
	}
	return facts, nil
}

func trimRight(s, cut string) string {
	for len(s) > 0 && bytes.HasSuffix([]byte(s), []byte(cut)) {
		s = s[:len(s)-len(cut)]
	}
	return s
}
