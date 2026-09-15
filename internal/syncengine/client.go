package syncengine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrPaymentRequired signals the backend's paid-tier gate (HTTP 402): sync
// is Veda's first paid feature; the local product stays free forever.
var ErrPaymentRequired = fmt.Errorf("sync is a paid feature — check your plan or sync token")

// Client speaks the sync backend's REST contract (implemented by the
// external Supabase repo; see DESIGN/12):
//
//	POST {url}/bundles                          -> {"id": <n>}
//	GET  {url}/bundles?since_id=N&device_id=X   -> {"bundles": [{"id", "device_id", "envelope"}]}
//
// Both take a Bearer token. GET never returns device X's own bundles.
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}

func NewClient(url, token string) *Client {
	return &Client{URL: strings.TrimRight(url, "/"), Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type pushResponse struct {
	ID int64 `json:"id"`
}

// Push stores one sealed envelope, returning the server-assigned bundle id.
func (c *Client) Push(ctx context.Context, env Envelope) (int64, error) {
	body, err := json.Marshal(env)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/bundles", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	c.authorize(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPaymentRequired {
		return 0, ErrPaymentRequired
	}
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("sync push returned %d", resp.StatusCode)
	}
	var pr pushResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return 0, err
	}
	return pr.ID, nil
}

// Received is one other device's bundle, with the server cursor to resume from.
type Received struct {
	ID       int64    `json:"id"`
	DeviceID string   `json:"device_id"`
	Envelope Envelope `json:"envelope"`
}

type pullResponse struct {
	Bundles []Received `json:"bundles"`
}

// Pull fetches other devices' bundles newer than sinceID (server-assigned,
// globally monotonic ids).
func (c *Client) Pull(ctx context.Context, sinceID int64, deviceID string) ([]Received, error) {
	url := fmt.Sprintf("%s/bundles?since_id=%d&device_id=%s", c.URL, sinceID, deviceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.authorize(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPaymentRequired {
		return nil, ErrPaymentRequired
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("sync pull returned %d", resp.StatusCode)
	}
	var pr pullResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}
	return pr.Bundles, nil
}

func (c *Client) authorize(req *http.Request) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Content-Type", "application/json")
}

var _ = io.Discard // keep io imported for future streaming pulls
