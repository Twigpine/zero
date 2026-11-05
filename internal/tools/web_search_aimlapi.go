package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/aimlapi"
	"github.com/Twigpine/zero/internal/redaction"
)

const (
	// aimlapiSearchProvider is the ZERO_WEBSEARCH_PROVIDER value that selects the
	// AI/ML API Toolbox. It is the only way to select it: every run is billed to
	// the user, so a configured AI/ML API key never enables it by itself.
	aimlapiSearchProvider = "aimlapi"
	aimlapiSearchTool     = "web/search"
	// aimlapiSearchMaxCostUSD caps the spend of one search. web/search costs
	// $0.0013 per result, at least $0.0091 and at most $0.0221 (20 results), so
	// the cap always covers a run; the Toolbox refuses a run it would exceed
	// before billing it.
	aimlapiSearchMaxCostUSD = 0.025

	// Toolbox searches typically finish in about 2-3 s, but a run can queue.
	// wait_ms asks the Toolbox to hold the POST open until the run ends or that
	// time passes, so most searches need no polling; the rest of the budget is
	// left for polling a run that is still queued. The generic backends keep
	// webSearchTimeout.
	aimlapiSearchTimeout = 30 * time.Second
	aimlapiSearchWaitMS  = 20_000

	aimlapiDefaultPollDelay  = time.Second
	aimlapiMinPollDelay      = 250 * time.Millisecond
	aimlapiMaxPollDelay      = 5 * time.Second
	aimlapiMaxErrorTextRunes = 300
)

// aimlapiSearchBackend runs the Toolbox tool web/search. The destination is
// the fixed aimlapi.ToolboxBaseURL (ZERO_WEBSEARCH_BASE_URL is ignored), and
// the key is the AI/ML API service key, resolved again on every search so a
// provider change is seen by the next call.
type aimlapiSearchBackend struct {
	client  *http.Client
	loadKey func() (string, error)
}

// newAIMLAPISearchBackend builds the backend; a nil transport means
// http.DefaultTransport.
func newAIMLAPISearchBackend(transport http.RoundTripper, loadKey func() (string, error)) *aimlapiSearchBackend {
	return &aimlapiSearchBackend{
		client: &http.Client{
			Transport: transport,
			Timeout:   aimlapiSearchTimeout,
			// Never follow a redirect: the key and the attribution headers must
			// reach no host but the Toolbox.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("refusing redirect from the AI/ML API Toolbox")
			},
		},
		loadKey: loadKey,
	}
}

func (backend *aimlapiSearchBackend) searchTimeout() time.Duration {
	return aimlapiSearchTimeout
}

func (backend *aimlapiSearchBackend) Search(ctx context.Context, query string, limit int) ([]searchResult, error) {
	return backend.searchDomains(ctx, query, limit, nil)
}

// searchDomains passes the normalized allowlist as include_domains so the
// Toolbox filters server-side; Run still applies filterWebSearchByDomains.
func (backend *aimlapiSearchBackend) searchDomains(ctx context.Context, query string, limit int, domains []string) ([]searchResult, error) {
	key, err := backend.loadKey()
	if err != nil {
		return nil, fmt.Errorf("AI/ML API search is unavailable: %w", err)
	}
	idempotencyKey, err := aimlapi.NewPaymentSessionID()
	if err != nil {
		return nil, fmt.Errorf("generate idempotency key: %w", err)
	}
	input := map[string]any{"query": query, "max_results": limit}
	if len(domains) > 0 {
		input["include_domains"] = domains
	}
	payload, err := json.Marshal(map[string]any{
		"tool":         aimlapiSearchTool,
		"input":        input,
		"max_cost_usd": aimlapiSearchMaxCostUSD,
		"wait_ms":      aimlapiSearchWaitMS,
		// A fresh key per search: the Toolbox coalesces a retried POST onto the
		// same run instead of billing a second one.
		"idempotency_key": idempotencyKey,
	})
	if err != nil {
		return nil, fmt.Errorf("encode AI/ML API search request: %w", err)
	}

	run, err := backend.do(ctx, http.MethodPost, aimlapi.ToolboxBaseURL+"/run", payload, key)
	if err != nil {
		return nil, err
	}
	for !aimlapiTerminalRunStatus(run.Status) && !run.IsError {
		if strings.TrimSpace(run.ID) == "" {
			return nil, errors.New("AI/ML API search returned a queued run without an id")
		}
		timer := time.NewTimer(aimlapiPollDelay(run.NextPollHintMS))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("AI/ML API search run %s did not finish: %w", aimlapiErrorText(run.ID, key), ctx.Err())
		case <-timer.C:
		}
		run, err = backend.do(ctx, http.MethodGet, aimlapi.ToolboxBaseURL+"/runs/"+url.PathEscape(run.ID), nil, key)
		if err != nil {
			return nil, err
		}
	}
	return aimlapiRunResults(run, key)
}

type aimlapiErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

type aimlapiRun struct {
	ID             string            `json:"id"`
	Status         string            `json:"status"`
	IsError        bool              `json:"is_error"`
	NextPollHintMS *float64          `json:"next_poll_hint_ms"`
	Error          *aimlapiErrorBody `json:"error"`
	Output         *struct {
		Results []struct {
			URL     string `json:"url"`
			Title   string `json:"title"`
			Snippet string `json:"snippet"`
		} `json:"results"`
	} `json:"output"`
}

// do sends one Toolbox request and decodes the run object it returns.
func (backend *aimlapiSearchBackend) do(ctx context.Context, method, target string, payload []byte, key string) (aimlapiRun, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	request, err := newAIMLAPIToolboxRequest(ctx, method, target, body, key)
	if err != nil {
		return aimlapiRun{}, err
	}
	response, err := backend.client.Do(request)
	if err != nil {
		// Report the cause without the *url.Error around it: that quotes the
		// request URL, and a polling URL carries the run id the Toolbox returned
		// percent-escaped, a form of any key echoed in it that scrubbing cannot
		// match.
		cause := err
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			cause = urlErr.Err
		}
		return aimlapiRun{}, fmt.Errorf("AI/ML API search request failed: %s", aimlapiErrorText(cause.Error(), key))
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, webSearchBodyLimit))
	if err != nil {
		return aimlapiRun{}, fmt.Errorf("read AI/ML API search response: %s", aimlapiErrorText(err.Error(), key))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return aimlapiRun{}, aimlapiHTTPError(response, raw, key)
	}
	var run aimlapiRun
	if err := json.Unmarshal(raw, &run); err != nil {
		return aimlapiRun{}, fmt.Errorf("AI/ML API search returned an invalid response (HTTP %d)", response.StatusCode)
	}
	return run, nil
}

// newAIMLAPIToolboxRequest builds a Toolbox request and attaches the key and
// the attribution headers only after checking that target is an aimlapi.com
// service URL; any other target is refused before anything is sent.
func newAIMLAPIToolboxRequest(ctx context.Context, method, target string, body io.Reader, key string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("build AI/ML API search request: %w", err)
	}
	if !aimlapi.IsAttributedURL(request.URL) {
		return nil, fmt.Errorf("AI/ML API search refused to send credentials to %q", request.URL.Redacted())
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("User-Agent", "zero-web-search/0.1")
	request.Header.Set("Authorization", "Bearer "+key)
	aimlapi.ApplyAttributionHeaders(request.Header, request.URL)
	return request, nil
}

// aimlapiHTTPError reports a refused request by status, error code, message
// and request id. The raw body is never surfaced, and the key is scrubbed from
// every field taken from it.
func aimlapiHTTPError(response *http.Response, raw []byte, key string) error {
	var envelope struct {
		Error *aimlapiErrorBody `json:"error"`
	}
	_ = json.Unmarshal(raw, &envelope)
	var parts aimlapiErrorBody
	if envelope.Error != nil {
		parts = *envelope.Error
	}
	text := fmt.Sprintf("AI/ML API search error HTTP %d", response.StatusCode)
	if code := aimlapiErrorText(parts.Code, key); code != "" {
		text += " " + code
	}
	switch {
	case response.StatusCode == http.StatusUnauthorized:
		text += " (check the AI/ML API key)"
	case parts.Code == "insufficient_funds":
		text += " (top up your AI/ML API balance)"
	case response.StatusCode == http.StatusTooManyRequests:
		if seconds, err := strconv.Atoi(strings.TrimSpace(response.Header.Get("Retry-After"))); err == nil && seconds > 0 {
			text += fmt.Sprintf(" (rate limited; retry after %ds)", seconds)
		} else {
			text += " (rate limited)"
		}
	}
	if message := aimlapiErrorText(parts.Message, key); message != "" {
		text += ": " + message
	}
	if requestID := aimlapiErrorText(parts.RequestID, key); requestID != "" {
		text += " (request_id: " + requestID + ")"
	}
	return errors.New(text)
}

// aimlapiRunResults maps a finished run onto search results. A run that ended
// in any status but completed, or reports is_error (which a completed run can),
// is a failure.
func aimlapiRunResults(run aimlapiRun, key string) ([]searchResult, error) {
	status := strings.TrimSpace(run.Status)
	if status == "" {
		status = "unknown"
	}
	if run.IsError || status != "completed" {
		text := fmt.Sprintf("AI/ML API %s run %s", aimlapiSearchTool, aimlapiErrorText(status, key))
		if run.IsError && status == "completed" {
			text += " with an error"
		}
		if run.Error != nil {
			if code := aimlapiErrorText(run.Error.Code, key); code != "" {
				text += " " + code
			}
			if message := aimlapiErrorText(run.Error.Message, key); message != "" {
				text += ": " + message
			}
		}
		if id := aimlapiErrorText(run.ID, key); id != "" {
			text += " (run_id: " + id + ")"
		}
		return nil, errors.New(text)
	}
	if run.Output == nil || run.Output.Results == nil {
		return nil, fmt.Errorf("AI/ML API %s returned no results list", aimlapiSearchTool)
	}
	out := make([]searchResult, 0, len(run.Output.Results))
	for _, item := range run.Output.Results {
		if strings.TrimSpace(item.URL) == "" {
			continue
		}
		out = append(out, searchResult{Title: scrubAIMLAPIResult(item.Title, key), URL: scrubAIMLAPIResult(item.URL, key), Snippet: scrubAIMLAPIResult(item.Snippet, key)})
	}
	return out, nil
}

func aimlapiTerminalRunStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case "completed", "failed", "stopped", "timed_out":
		return true
	}
	return false
}

// aimlapiPollDelay honours next_poll_hint_ms, clamped to 250 ms..5 s.
func aimlapiPollDelay(hintMS *float64) time.Duration {
	if hintMS == nil {
		return aimlapiDefaultPollDelay
	}
	delay := time.Duration(*hintMS * float64(time.Millisecond))
	if delay < aimlapiMinPollDelay {
		return aimlapiMinPollDelay
	}
	if delay > aimlapiMaxPollDelay {
		return aimlapiMaxPollDelay
	}
	return delay
}

// aimlapiErrorText scrubs the key from a field taken from a response, then
// collapses whitespace and clips it. Scrubbing runs first so clipping can never
// leave a fragment of the key behind.
func aimlapiErrorText(value, key string) string {
	text := redaction.RedactString(value, redaction.Options{ExtraSecretValues: []string{key}})
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > aimlapiMaxErrorTextRunes {
		text = string(runes[:aimlapiMaxErrorTextRunes]) + "…"
	}
	return text
}

// scrubAIMLAPIResult redacts the active credential without clipping page content.
func scrubAIMLAPIResult(value, key string) string {
	return redaction.RedactString(value, redaction.Options{ExtraSecretValues: []string{key}})
}
