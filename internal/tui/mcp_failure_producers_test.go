package tui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/tools"
)

// THESE FAILURES ARE PRODUCED, NOT CONSTRUCTED.
//
// Every test below registers a server through mcp.RegisterTools with nothing
// substituted for the connection, so the error is the one the real HTTP client
// or the real stdio launcher returns, wrapped the way startup wraps it. A fixture that starts from
// an errors.New around the expected text cannot see a boundary that sits in the
// producer: the 1024 bytes the HTTP reader keeps, or the stderr a child writes
// about the argument it was handed.

const mcpEchoHelperEnv = "ZERO_TUI_MCP_ECHO"

// TestMCPEchoHelperProcess is not a test. It is the stdio child the tests below
// start: it reports something about the endpoint it was handed and exits before
// the handshake, which is an initialization failure with stderr attached.
//
// The modes are deliberately short. Environment values are configuration, a long
// one would be collected as a candidate, and the word would then disappear from
// the message under test.
func TestMCPEchoHelperProcess(t *testing.T) {
	mode := os.Getenv(mcpEchoHelperEnv)
	if mode == "" {
		return
	}
	endpoint := ""
	for index, arg := range os.Args {
		if arg == "--" && index+1 < len(os.Args) {
			endpoint = os.Args[index+1]
			break
		}
	}
	_, authority, _ := strings.Cut(endpoint, "//")
	userinfo, _, _ := strings.Cut(authority, "@")
	username, password, _ := strings.Cut(userinfo, ":")
	_, fragment, _ := strings.Cut(endpoint, "#")
	_, fragmentValue, _ := strings.Cut(fragment, "=")
	unescape := func(value string) string {
		if decoded, err := url.PathUnescape(value); err == nil {
			return decoded
		}
		return value
	}
	switch mode {
	case "url":
		fmt.Fprintf(os.Stderr, "bridge: cannot reach %s\n", endpoint)
	case "raw":
		fmt.Fprintf(os.Stderr, "bridge: upstream refused %s\n", password)
	case "dec":
		fmt.Fprintf(os.Stderr, "bridge: upstream refused %s\n", unescape(password))
	case "usr":
		fmt.Fprintf(os.Stderr, "bridge: upstream refused %s\n", username)
	case "fdec":
		fmt.Fprintf(os.Stderr, "bridge: upstream refused %s\n", unescape(fragmentValue))
	}
	os.Exit(1)
}

// registerAndCollectSkipped runs the real registration and returns what startup
// would have recorded. wantRaw has to be in the RAW error, or the run proved
// nothing: a child that was slow to start, or a server that answered something
// else, would let every "is absent" assertion below pass on an empty message.
func registerAndCollectSkipped(t *testing.T, cfg config.MCPConfig, wantRaw string) []mcp.SkippedServer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	runtime, err := mcp.RegisterTools(ctx, tools.NewRegistry(), cfg, mcp.RegisterOptions{ConnectTimeout: time.Minute})
	if err != nil {
		t.Fatalf("RegisterTools() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	skipped := runtime.Skipped()
	if len(skipped) != 1 || skipped[0].Err == nil {
		t.Fatalf("skipped = %#v, want exactly one recorded failure", skipped)
	}
	if !strings.Contains(skipped[0].Err.Error(), wantRaw) {
		t.Fatalf("the producer did not report %q, so the redaction below is not exercised: %v", wantRaw, skipped[0].Err)
	}
	return skipped
}

func echoingStdioConfig(mode, endpoint string) config.MCPConfig {
	return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {
			Type:    "stdio",
			Command: os.Args[0],
			Args:    []string{"-test.run=TestMCPEchoHelperProcess", "--", endpoint},
			Env:     map[string]string{mcpEchoHelperEnv: mode},
		},
	}}
}

// surfaceRow returns the one rendered row that holds marker, so the Error row
// and the Target row are judged separately. They are built by different code,
// and a check on the whole panel lets either one hide behind the other.
func surfaceRow(t *testing.T, surface, text, marker string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, marker) {
			return line
		}
	}
	t.Fatalf("%s has no row containing %q:\n%s", surface, marker, text)
	return ""
}

// THE CUT DOES NOT HAVE TO BE OURS.
//
// The HTTP client keeps 1024 bytes of a failing response body. A body that
// spends 1000 of them on erase-line sequences and then echoes a configured
// header value leaves the first 24 bytes of a 32-byte credential at the end of
// an error that is far under this package's own bound. Exact-value redaction
// cannot match 24 of 32 bytes, normalization removes the sequences, and the
// prefix is what the reader is left looking at.
func TestCredentialPrefixCutByTheHTTPReaderIsNotDisplayed(t *testing.T) {
	const credential = "Qw7ZmPr4aBcD9eFgH2jKlM6nOpR8sTuV"
	const survivingPrefix = "Qw7ZmPr4aBcD9eFgH2jKlM6n"
	eraseLines := strings.Repeat("\x1b[2K", 250)

	for _, tc := range []struct {
		name    string
		body    string
		wantRaw string
		absent  []string
		present []string
	}{
		{
			name:    "credential sliced by the response reader",
			body:    eraseLines + credential,
			wantRaw: survivingPrefix,
			// The shortest recoverable run is checked too: dropping the last few
			// bytes of the prefix would still leave most of the credential.
			absent:  []string{survivingPrefix, survivingPrefix[:8]},
			present: []string{"returned HTTP 500"},
		},
		{
			name:    "control: an ordinary error is shown whole",
			body:    "workspace is not provisioned in this region",
			wantRaw: "not provisioned",
			present: []string{"returned HTTP 500", "workspace is not provisioned in this region"},
		},
		{
			name:    "control: a sliced body that is not a credential keeps its tail",
			body:    eraseLines + "upstream gateway timed out while contacting the workspace service",
			wantRaw: "upstream gateway timed",
			present: []string{"returned HTTP 500", "upstream gateway timed"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"docs": {Type: "http", URL: server.URL + "/mcp", Headers: map[string]string{"X-Api-Key": credential}},
			}}
			skipped := registerAndCollectSkipped(t, cfg, tc.wantRaw)
			panel, detail := failedServerSurfaces(t, cfg, "docs", skipped)
			for surface, text := range map[string]string{"panel": panel, "manager detail": detail} {
				for _, absent := range tc.absent {
					if strings.Contains(text, absent) {
						t.Errorf("%s shows %q of the credential:\n%s", surface, absent, text)
					}
				}
				for _, present := range tc.present {
					if !strings.Contains(text, present) {
						t.Errorf("%s lost %q:\n%s", surface, present, text)
					}
				}
			}
		})
	}
}

// A STDIO CHILD IS HANDED THE WHOLE ARGUMENT, FRAGMENT INCLUDED.
//
// No HTTP server ever sees a fragment, but the child does, and one that prints
// its endpoint while failing puts it into the stderr this panel renders. Target
// masked the value and the Error row one line above printed it.
func TestFragmentCredentialEchoedByAStdioChildIsRedacted(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
		mode     string
		wantRaw  string
		absent   []string
		readable []string
	}{
		{
			name:     "whole endpoint echoed",
			endpoint: "https://host.invalid/mcp#workspace=opaque-fragment-9f3c2b7ae1d8",
			mode:     "url",
			wantRaw:  "opaque-fragment-9f3c2b7ae1d8",
			absent:   []string{"opaque-fragment-9f3c2b7ae1d8"},
		},
		{
			name:     "escaped spelling echoed as it was handed over",
			endpoint: "https://host.invalid/mcp#workspace=opaque%2Dfragment%2D9f3c2b7ae1d8",
			mode:     "url",
			wantRaw:  "opaque%2Dfragment%2D9f3c2b7ae1d8",
			absent:   []string{"opaque%2Dfragment%2D9f3c2b7ae1d8", "opaque-fragment-9f3c2b7ae1d8"},
		},
		{
			name:     "escaped spelling configured, decoded value echoed alone",
			endpoint: "https://host.invalid/mcp#workspace=opaque%2Dfragment%2D9f3c2b7ae1d8",
			mode:     "fdec",
			wantRaw:  "opaque-fragment-9f3c2b7ae1d8",
			absent:   []string{"opaque-fragment-9f3c2b7ae1d8"},
		},
		{
			name:     "a short value under a credential key is still a credential",
			endpoint: "https://host.invalid/mcp#token=abc12",
			mode:     "url",
			wantRaw:  "abc12",
			absent:   []string{"abc12"},
		},
		{
			name:     "control: short fragment metadata stays readable",
			endpoint: "https://host.invalid/mcp#mode=sse",
			mode:     "url",
			wantRaw:  "mode=sse",
			readable: []string{"mode=sse"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := echoingStdioConfig(tc.mode, tc.endpoint)
			skipped := registerAndCollectSkipped(t, cfg, tc.wantRaw)
			panel, detail := failedServerSurfaces(t, cfg, "docs", skipped)
			for surface, text := range map[string]string{"panel": panel, "manager detail": detail} {
				errorRow := surfaceRow(t, surface, text, "bridge:")
				targetRow := surfaceRow(t, surface, text, "-test.run=TestMCPEchoHelperProcess")
				for label, row := range map[string]string{"Error": errorRow, "Target": targetRow} {
					for _, absent := range tc.absent {
						if strings.Contains(row, absent) {
							t.Errorf("%s %s row shows %q:\n%s", surface, label, absent, row)
						}
					}
					for _, readable := range tc.readable {
						if !strings.Contains(row, readable) {
							t.Errorf("%s %s row lost %q:\n%s", surface, label, readable, row)
						}
					}
					if !strings.Contains(row, "host.invalid/mcp") && (label == "Target" || tc.mode == "url") {
						t.Errorf("%s %s row no longer names the endpoint:\n%s", surface, label, row)
					}
				}
			}
		})
	}
}

// THE ESCAPED PASSWORD IS THE SAME CREDENTIAL IN ANOTHER SPELLING.
//
// "https://u:%73crt@host" decodes to the password "scrt", which was known by
// position. The spelling that was actually configured went into the ambiguous
// list and the readability floor discarded its six bytes. A child that reports
// the password alone reports that spelling, with no URL around it for the
// generic userinfo masking to recognise, so the whole-URL case is only a control
// here: it passes with or without the raw password in the exact-secret set.
func TestEscapedUserinfoPasswordEchoedAloneIsRedacted(t *testing.T) {
	const endpoint = "https://bob:%73crt@host.invalid/mcp"
	for _, tc := range []struct {
		name     string
		mode     string
		wantRaw  string
		absent   []string
		readable []string
	}{
		{name: "raw password alone", mode: "raw", wantRaw: "%73crt", absent: []string{"%73crt"}},
		{name: "decoded password alone", mode: "dec", wantRaw: "scrt", absent: []string{"scrt"}},
		{name: "control: whole URL", mode: "url", wantRaw: "%73crt", absent: []string{"%73crt", "scrt"}},
		{name: "control: a short ordinary username stays readable", mode: "usr", wantRaw: "refused bob", readable: []string{"refused bob"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := echoingStdioConfig(tc.mode, endpoint)
			skipped := registerAndCollectSkipped(t, cfg, tc.wantRaw)
			panel, detail := failedServerSurfaces(t, cfg, "docs", skipped)
			for surface, text := range map[string]string{"panel": panel, "manager detail": detail} {
				for _, absent := range tc.absent {
					if strings.Contains(text, absent) {
						t.Errorf("%s shows %q:\n%s", surface, absent, text)
					}
				}
				for _, readable := range tc.readable {
					if !strings.Contains(text, readable) {
						t.Errorf("%s lost %q:\n%s", surface, readable, text)
					}
				}
				if !strings.Contains(text, "bridge:") {
					t.Errorf("%s lost the diagnostic:\n%s", surface, text)
				}
			}
		})
	}
}
