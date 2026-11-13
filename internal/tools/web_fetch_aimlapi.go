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
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Twigpine/zero/internal/aimlapi"
	"github.com/Twigpine/zero/internal/config"
)

// The AI/ML API Toolbox backend fetches the page as Markdown through a
// Toolbox tool run instead of a direct GET. Toolbox runs are billed to the
// user's AI/ML API balance, so it is opt-in only: it runs when
// ZERO_WEBFETCH_PROVIDER=aimlapi is set, never just because a key exists.
const (
	webFetchProviderEnv     = "ZERO_WEBFETCH_PROVIDER"
	webFetchProviderAIMLAPI = "aimlapi"

	// webFetchAIMLAPITool is the Toolbox tool web_fetch runs: the lowest fixed
	// price ($0.0013 per run), main-content Markdown that keeps links, and a 404
	// or unresolvable host comes back as an is_error run, reported as an error.
	webFetchAIMLAPITool = "search1api/crawl"
	// webFetchAIMLAPIMaxCostUSD is sent with every run. It leaves room for a
	// small price change; a run quoted above it is refused before it starts
	// (400 max_cost_exceeded) instead of being charged.
	webFetchAIMLAPIMaxCostUSD = 0.005
	// webFetchAIMLAPIWaitMS is how long POST /run may hold the request open
	// before answering 202 with a run to poll.
	webFetchAIMLAPIWaitMS = 30000

	// webFetchAIMLAPITimeout bounds the whole scrape, retries and polling
	// included. Each request is also bounded, with room for wait_ms.
	webFetchAIMLAPITimeout        = 120 * time.Second
	webFetchAIMLAPIRequestTimeout = 60 * time.Second
	webFetchAIMLAPIMaxAttempts    = 3
	webFetchAIMLAPIBackoff        = 500 * time.Millisecond
	webFetchAIMLAPIMaxRetryDelay  = 10 * time.Second
	webFetchAIMLAPIDefaultPoll    = time.Second
	webFetchAIMLAPIMinPoll        = 250 * time.Millisecond
	webFetchAIMLAPIMaxPoll        = 5 * time.Second
	// webFetchAIMLAPIResponseLimit bounds a run object; it holds at most a page
	// of Markdown, which max_bytes then cuts to the caller's budget.
	webFetchAIMLAPIResponseLimit = 8 * 1024 * 1024
	// webFetchAIMLAPIErrorReadLimit is read from an error body before the key
	// is scrubbed; the shown text is cut to webFetchErrorBodyLimit afterwards,
	// so the cut can never leave a fragment of the key behind.
	webFetchAIMLAPIErrorReadLimit = 64 * 1024
)

var errWebFetchAIMLAPIRedirect = errors.New("the AI/ML API Toolbox answered with a redirect; redirects are refused so the key never leaves tools.aimlapi.com")

// webFetchAIMLAPIBackend holds the seams of the Toolbox backend. baseURL is
// always aimlapi.ToolboxBaseURL outside tests; it is not configurable.
type webFetchAIMLAPIBackend struct {
	client         *http.Client
	baseURL        string
	serviceKey     func() (string, error)
	idempotencyKey func() (string, error)
	sleep          func(context.Context, time.Duration) error
}

func defaultWebFetchAIMLAPIBackend() webFetchAIMLAPIBackend {
	return webFetchAIMLAPIBackend{
		baseURL:    aimlapi.ToolboxBaseURL,
		serviceKey: config.LoadAIMLAPIServiceKey,
		// A UUIDv4 generator; it is used here for the run's idempotency key.
		idempotencyKey: aimlapi.NewPaymentSessionID,
		sleep:          sleepWebFetchContext,
	}
}

// webFetchAIMLAPISelected reads ZERO_WEBFETCH_PROVIDER on every call. Unset,
// empty and unknown values keep the direct fetch.
func webFetchAIMLAPISelected() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(webFetchProviderEnv)), webFetchProviderAIMLAPI)
}

type webFetchToolboxRunRequest struct {
	Tool           string                  `json:"tool"`
	Input          webFetchToolboxRunInput `json:"input"`
	MaxCostUSD     float64                 `json:"max_cost_usd"`
	WaitMS         int                     `json:"wait_ms"`
	IdempotencyKey string                  `json:"idempotency_key"`
}

type webFetchToolboxRunInput struct {
	URL string `json:"url"`
}

type webFetchToolboxRun struct {
	ID             string          `json:"id"`
	Status         string          `json:"status"`
	IsError        bool            `json:"is_error"`
	Output         json.RawMessage `json:"output"`
	Error          json.RawMessage `json:"error"`
	NextPollHintMS *float64        `json:"next_poll_hint_ms"`
	Cost           struct {
		ActualUSD *float64 `json:"actual_usd"`
		OverMax   bool     `json:"over_max"`
	} `json:"cost"`
}

type webFetchToolboxRunError struct {
	Message   string `json:"message"`
	Code      string `json:"code"`
	Kind      string `json:"kind"`
	Retryable bool   `json:"retryable"`
}

// runAIMLAPI fetches target through the Toolbox. Any failure is an error
// result: there is no fallback to a direct fetch.
func (tool webFetchTool) runAIMLAPI(ctx context.Context, rawURL string, maxBytes int, format string) Result {
	// The pre-permission and resolved-host checks run before the URL leaves
	// the machine, so localhost, private and metadata URLs never reach a
	// third-party crawler. A resolver is required here: there is no dial-time
	// pin to fall back on, since the Toolbox dials the target.
	parsedURL, err := validateWebFetchURL(ctx, rawURL, firstWebFetchResolver(tool.resolver))
	if err != nil {
		return errorResult("Error: Unsafe URL for web_fetch: " + err.Error())
	}
	// The Toolbox returns the crawler's Markdown, never the original response,
	// so format "raw" cannot be honoured. Refusing it (before any billed run)
	// is clearer than returning Markdown the caller asked not to get.
	if format == "raw" {
		return webFetchAIMLAPIRawFormatError()
	}

	backend := tool.aimlapi
	key, err := backend.serviceKey()
	if err == nil && strings.TrimSpace(key) == "" {
		err = config.ErrAIMLAPIKeyMissing
	}
	if err != nil {
		return errorResult("Error: ZERO_WEBFETCH_PROVIDER=aimlapi needs a usable AI/ML API key: " + redactWebFetchText(scrubWebFetchKey(err.Error(), key)) + ". web_fetch does not fall back to a direct fetch; unset ZERO_WEBFETCH_PROVIDER to fetch pages directly.")
	}
	scrub := func(value string) string {
		return redactWebFetchText(scrubWebFetchKey(value, key))
	}

	run, statusCode, err := backend.scrape(ctx, key, parsedURL.String())
	if err != nil {
		return errorResult("Error fetching URL via AI/ML API Toolbox (" + webFetchAIMLAPITool + "): " + scrub(err.Error()))
	}
	capNote := ""
	if run.Cost.OverMax {
		capNote = fmt.Sprintf(" The run hit the $%g max_cost_usd cap.", webFetchAIMLAPIMaxCostUSD)
	}
	if run.IsError || run.Status != "completed" {
		reason := "run ended with status " + firstNonEmptyString(run.Status, "unknown")
		if runError := decodeWebFetchToolboxRunError(run.Error); runError.Message != "" {
			reason = runError.Message
			if runError.Retryable {
				reason += " (retryable)"
			}
		}
		return errorResult("Error fetching URL via AI/ML API Toolbox (" + webFetchAIMLAPITool + "): " + scrub(reason) + "." + capNote)
	}
	content := webFetchToolboxContent(run.Output)
	if strings.TrimSpace(content) == "" {
		return errorResult("Error fetching URL via AI/ML API Toolbox (" + webFetchAIMLAPITool + "): the run returned no page content." + capNote)
	}

	// Redact before cutting to max_bytes, so the cut cannot split a secret
	// into a fragment the patterns no longer match.
	body := scrub(content)
	truncated := false
	if len(body) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(body[cut]) {
			cut--
		}
		body = body[:cut]
		truncated = true
	}

	finalURL := redactWebFetchURL(parsedURL)
	headers := []string{
		"URL: " + finalURL,
		"Status: completed (AI/ML API Toolbox run)",
		"Content-Type: text/markdown",
		"Bytes: " + strconv.Itoa(len(body)),
		"Backend: " + webFetchProviderAIMLAPI + " (" + webFetchAIMLAPITool + ")",
	}
	meta := map[string]string{
		"url":          finalURL,
		"status_code":  strconv.Itoa(statusCode),
		"content_type": "text/markdown",
		"bytes":        strconv.Itoa(len(body)),
		"truncated":    strconv.FormatBool(truncated),
		"converted":    "false",
		"backend":      webFetchProviderAIMLAPI,
		"tool":         webFetchAIMLAPITool,
	}
	if run.ID != "" {
		meta["run_id"] = scrub(run.ID)
	}
	if run.Cost.ActualUSD != nil {
		meta["cost_usd"] = strconv.FormatFloat(*run.Cost.ActualUSD, 'f', -1, 64)
	}
	return Result{
		Status:    StatusOK,
		Output:    strings.Join(append(headers, "", body), "\n"),
		Truncated: truncated,
		Meta:      meta,
	}
}

// scrape starts one run and waits for it to finish. It returns the terminal
// run and the HTTP status of the response that carried it.
func (backend webFetchAIMLAPIBackend) scrape(ctx context.Context, key string, target string) (webFetchToolboxRun, int, error) {
	ctx, cancel := context.WithTimeout(ctx, webFetchAIMLAPITimeout)
	defer cancel()

	idempotencyKey, err := backend.idempotencyKey()
	if err != nil {
		return webFetchToolboxRun{}, 0, fmt.Errorf("create idempotency key: %w", err)
	}
	// One body for every attempt: a retry carries the same idempotency key, so
	// a run that did start is returned again instead of being billed twice.
	body, err := json.Marshal(webFetchToolboxRunRequest{
		Tool:           webFetchAIMLAPITool,
		Input:          webFetchToolboxRunInput{URL: target},
		MaxCostUSD:     webFetchAIMLAPIMaxCostUSD,
		WaitMS:         webFetchAIMLAPIWaitMS,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return webFetchToolboxRun{}, 0, err
	}
	client := backend.httpClient()

	var run webFetchToolboxRun
	statusCode := 0
	for attempt := 1; ; attempt++ {
		response, err := backend.send(ctx, &client, http.MethodPost, backend.baseURL+"/run", body, key)
		if err != nil {
			return webFetchToolboxRun{}, 0, err
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			run, err = decodeWebFetchToolboxRun(response)
			if err != nil {
				return webFetchToolboxRun{}, 0, err
			}
			statusCode = response.StatusCode
			break
		}
		// 400 max_cost_exceeded and other refusals are final; only rate limits
		// and gateway errors are retried.
		if !webFetchAIMLAPIRetryable(response.StatusCode) || attempt >= webFetchAIMLAPIMaxAttempts {
			return webFetchToolboxRun{}, 0, webFetchToolboxRequestError(response, "run", key)
		}
		delay := webFetchAIMLAPIRetryDelay(response.Header.Get("Retry-After"), attempt)
		discardWebFetchBody(response)
		if err := backend.sleep(ctx, delay); err != nil {
			return webFetchToolboxRun{}, 0, err
		}
	}

	pollRetryDelay := time.Duration(0)
	for !webFetchToolboxTerminal(run.Status) {
		if run.ID == "" {
			return webFetchToolboxRun{}, 0, errors.New("the run is still pending but has no run id")
		}
		if err := backend.sleep(ctx, max(webFetchAIMLAPIPollDelay(run.NextPollHintMS), pollRetryDelay)); err != nil {
			return webFetchToolboxRun{}, 0, err
		}
		response, err := backend.send(ctx, &client, http.MethodGet, backend.baseURL+"/runs/"+url.PathEscape(run.ID), nil, key)
		if err != nil {
			return webFetchToolboxRun{}, 0, err
		}
		pollRetryDelay = 0
		if webFetchAIMLAPIRetryable(response.StatusCode) {
			if retryAfter := strings.TrimSpace(response.Header.Get("Retry-After")); retryAfter != "" {
				pollRetryDelay = webFetchAIMLAPIRetryDelay(retryAfter, 1)
			}
			discardWebFetchBody(response)
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return webFetchToolboxRun{}, 0, webFetchToolboxRequestError(response, "run status", key)
		}
		next, err := decodeWebFetchToolboxRun(response)
		if err != nil {
			return webFetchToolboxRun{}, 0, err
		}
		if next.ID == "" {
			next.ID = run.ID
		}
		run, statusCode = next, response.StatusCode
	}
	return run, statusCode, nil
}

// httpClient is a plain client for the fixed Toolbox endpoint. It does not use
// the SSRF-pinning transport meant for target sites, and like Zero's other
// aimlapi.com clients it uses the default transport, which honours
// HTTP_PROXY/HTTPS_PROXY/NO_PROXY.
func (backend webFetchAIMLAPIBackend) httpClient() http.Client {
	client := http.Client{Timeout: webFetchAIMLAPIRequestTimeout}
	if backend.client != nil {
		client = *backend.client
	}
	// Never follow a redirect: the key and attribution headers must not reach
	// any host but the Toolbox.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errWebFetchAIMLAPIRedirect
	}
	return client
}

func (backend webFetchAIMLAPIBackend) send(ctx context.Context, client *http.Client, method string, endpoint string, body []byte, key string) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	// Fail closed: the key and attribution go only to an aimlapi.com service.
	if !aimlapi.IsAttributedURL(request.URL) {
		return nil, errors.New("refusing to send the AI/ML API key to a host other than tools.aimlapi.com")
	}
	request.Header.Set("Authorization", "Bearer "+key)
	aimlapi.ApplyAttributionHeaders(request.Header, request.URL)
	request.Header.Set("User-Agent", "zero-web-fetch/0.1")
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		// Report the cause without the *url.Error around it, as web_search
		// does: that quotes the request URL, and a polling URL carries the run
		// id the Toolbox returned percent-escaped, a form of any key echoed in
		// it that scrubbing cannot match.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return nil, urlErr.Err
		}
		return nil, err
	}
	if response.Body == nil {
		response.Body = io.NopCloser(strings.NewReader(""))
	}
	return response, nil
}

func decodeWebFetchToolboxRun(response *http.Response) (webFetchToolboxRun, error) {
	defer response.Body.Close()
	raw, truncated, err := readWebFetchBody(response.Body, webFetchAIMLAPIResponseLimit)
	if err != nil {
		return webFetchToolboxRun{}, fmt.Errorf("read the run: %w", err)
	}
	if truncated {
		return webFetchToolboxRun{}, fmt.Errorf("the run response is larger than %d bytes", webFetchAIMLAPIResponseLimit)
	}
	var run webFetchToolboxRun
	if err := json.Unmarshal([]byte(raw), &run); err != nil {
		return webFetchToolboxRun{}, errors.New("the run response is not valid JSON")
	}
	return run, nil
}

// webFetchToolboxRequestError reports a non-2xx answer with the error envelope
// the Toolbox sends ({"error":{"code","message","request_id"}}). The key is
// scrubbed from the decoded text before it is cut to size.
func webFetchToolboxRequestError(response *http.Response, action string, key string) error {
	defer response.Body.Close()
	raw, _, _ := readWebFetchBody(response.Body, webFetchAIMLAPIErrorReadLimit)
	var envelope struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	detail := ""
	if json.Unmarshal([]byte(raw), &envelope) == nil && (envelope.Error.Code != "" || envelope.Error.Message != "") {
		if envelope.Error.Code != "" {
			detail += " (" + envelope.Error.Code + ")"
		}
		if envelope.Error.Message != "" {
			detail += ": " + envelope.Error.Message
		}
		if envelope.Error.RequestID != "" {
			detail += " [request_id " + envelope.Error.RequestID + "]"
		}
	} else if text := strings.TrimSpace(raw); text != "" {
		detail = ": " + text
	}
	detail = scrubWebFetchKey(detail, key)
	if len(detail) > webFetchErrorBodyLimit {
		detail = detail[:webFetchErrorBodyLimit]
	}
	message := "AI/ML API Toolbox " + action + " returned HTTP " + webFetchStatusLine(response) + detail
	if envelope.Error.Code == "max_cost_exceeded" {
		message += fmt.Sprintf(". The run was refused before it started (cap $%g) and was not billed", webFetchAIMLAPIMaxCostUSD)
	}
	return errors.New(message)
}

func decodeWebFetchToolboxRunError(raw json.RawMessage) webFetchToolboxRunError {
	var runError webFetchToolboxRunError
	if len(raw) == 0 {
		return runError
	}
	if json.Unmarshal(raw, &runError) == nil {
		if runError.Message == "" {
			runError.Message = firstNonEmptyString(runError.Code, runError.Kind)
		}
		return runError
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		runError.Message = text
	}
	return runError
}

// webFetchToolboxContent returns output.results.content, the crawler's
// Markdown, or "" when the output has another shape.
func webFetchToolboxContent(raw json.RawMessage) string {
	var output struct {
		Results struct {
			Content json.RawMessage `json:"content"`
		} `json:"results"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &output) != nil {
		return ""
	}
	var content string
	if json.Unmarshal(output.Results.Content, &content) != nil {
		return ""
	}
	return content
}

func webFetchToolboxTerminal(status string) bool {
	switch status {
	case "completed", "failed", "stopped", "timed_out":
		return true
	}
	return false
}

func webFetchAIMLAPIRetryable(statusCode int) bool {
	switch statusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// webFetchAIMLAPIRetryDelay honours Retry-After (seconds or an HTTP date) and
// otherwise backs off exponentially; either way it is capped.
func webFetchAIMLAPIRetryDelay(retryAfter string, attempt int) time.Duration {
	delay := webFetchAIMLAPIBackoff << (attempt - 1)
	if value := strings.TrimSpace(retryAfter); value != "" {
		if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
			// Capped before the multiplication, so a huge value cannot overflow.
			delay = time.Duration(min(seconds, int(webFetchAIMLAPIMaxRetryDelay/time.Second))) * time.Second
		} else if when, err := http.ParseTime(value); err == nil {
			delay = time.Until(when)
		}
	}
	return max(0, min(delay, webFetchAIMLAPIMaxRetryDelay))
}

// webFetchAIMLAPIPollDelay honours next_poll_hint_ms, clamped to 250 ms..5 s.
func webFetchAIMLAPIPollDelay(hintMS *float64) time.Duration {
	if hintMS == nil {
		return webFetchAIMLAPIDefaultPoll
	}
	// Clamp in milliseconds first, so a huge hint cannot overflow a Duration.
	ms := max(float64(webFetchAIMLAPIMinPoll/time.Millisecond), min(*hintMS, float64(webFetchAIMLAPIMaxPoll/time.Millisecond)))
	return time.Duration(ms) * time.Millisecond
}

// scrubWebFetchKey removes an echoed AI/ML API key from text shown to the
// model, whole or split by invisible characters, as web_search does. An empty
// key is no key: replacing "" would rewrite every byte gap.
func scrubWebFetchKey(value string, key string) string {
	if key == "" {
		return value
	}
	return scrubAIMLAPIResult(value, key)
}

func discardWebFetchBody(response *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, webFetchErrorBodyLimit))
	_ = response.Body.Close()
}

func sleepWebFetchContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// webFetchAIMLAPIRawFormatError is shared by preflight and execution.
func webFetchAIMLAPIRawFormatError() Result {
	return errorResult(`Error: format "raw" is not available with ZERO_WEBFETCH_PROVIDER=aimlapi: the AI/ML API Toolbox returns the page as Markdown, not the original response. Use format "auto" or "markdown", or unset ZERO_WEBFETCH_PROVIDER to fetch the page directly.`)
}
