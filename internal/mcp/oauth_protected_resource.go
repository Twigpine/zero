package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Gitlawb/zero/internal/oauth"
)

const protectedResourceMetadataLimit = 1 << 20

type protectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
}

// withoutDiscoveryRedirects returns a shallow client copy that exposes 3xx
// responses to the caller. Every discovered URL is validated before use, so a
// redirect must never replace that validated target with an unchecked one.
func withoutDiscoveryRedirects(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	copied := *client
	// Discovery requests are deliberately unauthenticated. A caller-supplied
	// jar must not turn a plain metadata GET into a credential-bearing request.
	copied.Jar = nil
	copied.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &copied
}

// discoverProtectedResourceAuthorizationServer follows the RFC 9728 URL
// advertised by a protected MCP resource. found is false when the endpoint does
// not advertise protected-resource metadata, allowing legacy direct discovery.
func discoverProtectedResourceAuthorizationServer(ctx context.Context, client *http.Client, resourceURL string) (issuer string, found bool, err error) {
	resourceURL = strings.TrimSpace(resourceURL)
	client = withoutDiscoveryRedirects(client)
	metadataURL, found, err := probeProtectedResourceMetadataURL(ctx, client, resourceURL)
	if err != nil || !found {
		return "", found, err
	}
	if err := validateResourceMetadataOrigin(resourceURL, metadataURL); err != nil {
		return "", true, err
	}
	client, err = newAdvertisedOAuthDiscoveryClient(client, resourceURL)
	if err != nil {
		return "", true, err
	}
	metadata, err := fetchProtectedResourceMetadata(ctx, client, metadataURL)
	if err != nil {
		return "", true, err
	}
	if metadata.Resource != resourceURL {
		return "", true, errors.New("mcp oauth: protected resource metadata resource does not match the MCP server URL")
	}
	if len(metadata.AuthorizationServers) == 0 {
		return "", true, errors.New("mcp oauth: protected resource metadata has no authorization_servers")
	}
	issuer = metadata.AuthorizationServers[0]
	if strings.TrimSpace(issuer) == "" {
		return "", true, errors.New("mcp oauth: protected resource metadata has an empty authorization server")
	}
	if issuer != strings.TrimSpace(issuer) {
		return "", true, errors.New("mcp oauth: protected resource authorization server must not contain surrounding whitespace")
	}
	if err := oauth.ValidateEndpointURL(issuer); err != nil {
		return "", true, fmt.Errorf("mcp oauth: protected resource authorization server: %w", err)
	}
	if err := validateAdvertisedOAuthDiscoveryURL(resourceURL, issuer); err != nil {
		return "", true, err
	}
	return issuer, true, nil
}

func validateResourceMetadataOrigin(resourceURL string, metadataURL string) error {
	resource, resourceErr := url.Parse(resourceURL)
	metadata, metadataErr := url.Parse(metadataURL)
	if resourceErr != nil || metadataErr != nil || !sameMCPOrigin(resource, metadata) {
		return errors.New("mcp oauth: protected resource metadata URL must be same-origin with the MCP server URL")
	}
	return nil
}

func probeProtectedResourceMetadataURL(ctx context.Context, client *http.Client, resourceURL string) (string, bool, error) {
	if client == nil {
		client = http.DefaultClient
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSpace(resourceURL), nil)
	if err != nil {
		return "", false, errors.New("mcp oauth: invalid MCP resource URL")
	}
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", defaultProtocolVersion)
	response, err := client.Do(request)
	if err != nil {
		// A resource that cannot be probed may still support the legacy direct
		// authorization-server discovery flow.
		return "", false, nil
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		return "", false, nil
	}
	metadataURL, found, err := parseResourceMetadataURL(response.Header.Values("WWW-Authenticate"))
	if err != nil {
		return "", false, fmt.Errorf("mcp oauth: invalid WWW-Authenticate resource_metadata parameter: %w", err)
	}
	if !found {
		return "", false, nil
	}
	if err := oauth.ValidateEndpointURL(metadataURL); err != nil {
		return "", false, fmt.Errorf("mcp oauth: protected resource metadata URL: %w", err)
	}
	return metadataURL, true, nil
}

func fetchProtectedResourceMetadata(ctx context.Context, client *http.Client, metadataURL string) (protectedResourceMetadata, error) {
	if client == nil {
		client = http.DefaultClient
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, nil)
	if err != nil {
		return protectedResourceMetadata{}, errors.New("mcp oauth: invalid protected resource metadata URL")
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return protectedResourceMetadata{}, errors.New("mcp oauth: fetch protected resource metadata failed")
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return protectedResourceMetadata{}, fmt.Errorf("mcp oauth: protected resource metadata returned HTTP %d", response.StatusCode)
	}
	var metadata protectedResourceMetadata
	if err := json.NewDecoder(io.LimitReader(response.Body, protectedResourceMetadataLimit)).Decode(&metadata); err != nil {
		return protectedResourceMetadata{}, errors.New("mcp oauth: decode protected resource metadata failed")
	}
	return metadata, nil
}

func parseResourceMetadataURL(headerValues []string) (string, bool, error) {
	var selected string
	for _, headerValue := range headerValues {
		for index := 0; index < len(headerValue); {
			if headerValue[index] == '"' {
				_, next, err := readAuthQuotedString(headerValue, index)
				if err != nil {
					// Malformed parameters for another authentication scheme do
					// not suppress a valid challenge in a separate header value.
					break
				}
				index = next
				continue
			}
			if !isAuthTokenByte(headerValue[index]) {
				index++
				continue
			}
			start := index
			for index < len(headerValue) && isAuthTokenByte(headerValue[index]) {
				index++
			}
			name := headerValue[start:index]
			for index < len(headerValue) && (headerValue[index] == ' ' || headerValue[index] == '\t') {
				index++
			}
			if index >= len(headerValue) || headerValue[index] != '=' {
				continue
			}
			index++
			for index < len(headerValue) && (headerValue[index] == ' ' || headerValue[index] == '\t') {
				index++
			}
			if !strings.EqualFold(name, "resource_metadata") {
				continue
			}
			if index >= len(headerValue) || headerValue[index] != '"' {
				return "", false, errors.New("resource_metadata must be a quoted URL")
			}
			value, next, err := readAuthQuotedString(headerValue, index)
			if err != nil {
				return "", false, err
			}
			value = strings.TrimSpace(value)
			if value == "" {
				return "", false, errors.New("resource_metadata URL is empty")
			}
			if selected != "" && selected != value {
				return "", false, errors.New("conflicting resource_metadata URLs")
			}
			selected = value
			index = next
		}
	}
	return selected, selected != "", nil
}

func readAuthQuotedString(value string, quote int) (string, int, error) {
	var decoded strings.Builder
	for index := quote + 1; index < len(value); index++ {
		switch value[index] {
		case '"':
			return decoded.String(), index + 1, nil
		case '\\':
			index++
			if index >= len(value) {
				return "", 0, errors.New("unterminated quoted resource_metadata URL")
			}
			decoded.WriteByte(value[index])
		default:
			if value[index] < 0x20 || value[index] == 0x7f {
				return "", 0, errors.New("resource_metadata URL contains a control character")
			}
			decoded.WriteByte(value[index])
		}
	}
	return "", 0, errors.New("unterminated quoted resource_metadata URL")
}

func isAuthTokenByte(value byte) bool {
	if value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' {
		return true
	}
	return strings.ContainsRune("!#$%&'*+-.^_`|~", rune(value))
}
