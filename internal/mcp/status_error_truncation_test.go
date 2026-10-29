package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// THE PRODUCER SAYS WHEN IT CUT, AND THE CUT STAYS AT THE END.
//
// A failing response body is bounded here, long before anything is displayed.
// The consumer that renders the error repairs a credential the cut sliced in
// half, but it can only do that if it is told a cut happened, and only at the
// place it looks, which is the tail. Both halves of that contract live in this
// package: the marker survives the wrapping the connect path adds, and that
// wrapping only ever prefixes.
func TestHTTPStatusErrorReportsWhenItCutTheBody(t *testing.T) {
	for _, tc := range []struct {
		name          string
		serverType    ServerType
		body          string
		interrupted   bool
		wantTruncated bool
		wantTail      string
	}{
		{"http, one byte over the limit", ServerTypeHTTP, strings.Repeat("a", maxHTTPErrorDetail-4) + "tail" + "X", false, true, "tail"},
		{"http, exactly the limit", ServerTypeHTTP, strings.Repeat("a", maxHTTPErrorDetail-4) + "tail", false, false, "tail"},
		{"http, short body", ServerTypeHTTP, "workspace is not provisioned", false, false, "provisioned"},
		{"http, empty body", ServerTypeHTTP, "", false, false, "HTTP 500"},
		{"sse, one byte over the limit", ServerTypeSSE, strings.Repeat("a", maxHTTPErrorDetail-4) + "tail" + "X", false, true, "tail"},
		// The connection closes long before the promised length, so the short
		// body that did arrive is partial even though it is under the limit.
		{"http, body interrupted under the limit", ServerTypeHTTP, "Qw7ZmPr4aBcD9eFgH2jKlM6n", true, true, "Qw7ZmPr4aBcD9eFgH2jKlM6n"},
		{"sse, body interrupted under the limit", ServerTypeSSE, "Qw7ZmPr4aBcD9eFgH2jKlM6n", true, true, "Qw7ZmPr4aBcD9eFgH2jKlM6n"},
		{"http, interrupted before any body", ServerTypeHTTP, "", true, true, "HTTP 500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.interrupted {
					writeInterruptedStatus(t, w, http.StatusInternalServerError, tc.body)
					return
				}
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			_, err := Connect(context.Background(), Server{Name: "docs", Type: tc.serverType, URL: server.URL})
			if err == nil {
				t.Fatal("Connect() succeeded against a server that answers 500")
			}
			if !strings.Contains(err.Error(), "HTTP 500") {
				t.Fatalf("error = %v, want the status error", err)
			}
			if got := ErrorDetailTruncated(err); got != tc.wantTruncated {
				t.Errorf("ErrorDetailTruncated = %v, want %v for %d body bytes", got, tc.wantTruncated, len(tc.body))
			}
			if !strings.HasSuffix(err.Error(), tc.wantTail) {
				t.Errorf("the retained body is no longer at the end of the error, which is the only place the display repairs a cut: %q", tailOf(err.Error(), 80))
			}
		})
	}
}

// writeInterruptedStatus answers with a status that promises a longer body than
// it sends, then drops the connection, so the client's read fails part way.
func writeInterruptedStatus(t *testing.T, w http.ResponseWriter, status int, body string) {
	t.Helper()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		t.Error("response writer cannot be hijacked")
		return
	}
	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		t.Errorf("Hijack() error = %v", err)
		return
	}
	defer conn.Close()
	_, _ = fmt.Fprintf(buffered, "HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s",
		status, http.StatusText(status), len(body)+2048, body)
	_ = buffered.Flush()
}

func TestErrorDetailTruncatedIsFalseForOtherErrors(t *testing.T) {
	if ErrorDetailTruncated(nil) {
		t.Error("nil reported as truncated")
	}
	if ErrorDetailTruncated(errors.New("dial tcp: connection refused")) {
		t.Error("an ordinary error reported as truncated")
	}
}

func tailOf(text string, size int) string {
	if len(text) <= size {
		return text
	}
	return text[len(text)-size:]
}
