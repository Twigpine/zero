package aimlapi

import (
	"net/http"
	"net/url"
	"testing"
)

func TestIsAttributedURLMatchesExactServiceHostsOnly(t *testing.T) {
	cases := map[string]bool{
		"https://tools.aimlapi.com/v1/tools/run":    true,
		"https://mcp.aimlapi.com/mcp":               true,
		"https://api.aimlapi.com/v1":                true,
		"https://TOOLS.aimlapi.com:443/v1/tools":    true,
		"http://tools.aimlapi.com/v1/tools/run":     false,
		"https://tools.aimlapi.com:8443/v1/tools":   false,
		"https://user@tools.aimlapi.com/v1/tools":   false,
		"https://tools.aimlapi.com.example.net/run": false,
		"https://eviltools.aimlapi.com/run":         false,
		"https://aimlapi.com/app":                   false,
		"https://auth.aimlapi.com/":                 false,
		"https://example.com/?h=tools.aimlapi.com":  false,
	}
	for raw, want := range cases {
		target, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		if got := IsAttributedURL(target); got != want {
			t.Errorf("IsAttributedURL(%q) = %v, want %v", raw, got, want)
		}
	}
	if IsAttributedURL(nil) {
		t.Fatal("IsAttributedURL(nil) = true")
	}
}

func TestApplyAttributionHeadersSetsSourceAndPartnerOnServiceHosts(t *testing.T) {
	t.Setenv("AIMLAPI_PARTNER_ID", "")
	header := http.Header{}
	ApplyAttributionHeaders(header, &url.URL{Scheme: "https", Host: "tools.aimlapi.com", Path: "/v1/tools/run"})
	if got := header.Get(SourceHeaderName); got != "agent/zero" {
		t.Fatalf("%s = %q, want agent/zero", SourceHeaderName, got)
	}
	if got := header.Get(PartnerHeaderName); got != DefaultPartnerID {
		t.Fatalf("%s = %q, want %q", PartnerHeaderName, got, DefaultPartnerID)
	}

	other := http.Header{}
	ApplyAttributionHeaders(other, &url.URL{Scheme: "https", Host: "tools.aimlapi.com.example.net"})
	if len(other) != 0 {
		t.Fatalf("attribution reached a non-aimlapi host: %#v", other)
	}
}
