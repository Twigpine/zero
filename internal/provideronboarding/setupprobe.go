package provideronboarding

import (
	"strings"

	"github.com/Twigpine/zero/internal/providerhealth"
)

// SetupProbeClass is the stable, machine-readable class of a first-run probe
// failure. It exists so the wizard can render a specific, fixable remedy instead
// of a raw stack trace: each class maps to one concrete thing the user changes.
type SetupProbeClass string

const (
	SetupProbeAuth      SetupProbeClass = "auth"       // bad / missing API key
	SetupProbeEndpoint  SetupProbeClass = "endpoint"   // wrong base URL, unreachable, timeout
	SetupProbeModel     SetupProbeClass = "model"      // model not found at the provider
	SetupProbeRateLimit SetupProbeClass = "rate_limit" // throttled; key works, retry later
	SetupProbeConfig    SetupProbeClass = "config"     // local profile is incomplete
	SetupProbeUnknown   SetupProbeClass = "unknown"    // unclassified provider error
)

// SetupProbeError is a classified, user-facing first-run probe failure. It is a
// value type (not panicking) so a failing probe can be rendered as a single
// fixable line. Message is the remedy; Detail is the underlying (already
// secret-scrubbed) provider message for context.
type SetupProbeError struct {
	Class   SetupProbeClass
	Message string
	Detail  string
}

func (err SetupProbeError) Error() string {
	if strings.TrimSpace(err.Detail) == "" {
		return err.Message
	}
	return err.Message + " (" + err.Detail + ")"
}

// ClassifySetupProbe maps a provider health probe result to a specific, fixable
// error. It returns ok=false when the probe passed (nothing to fix). It never
// panics: an empty or degenerate result classifies to SetupProbeUnknown rather
// than dereferencing a nil check.
//
// The whole point is that "paste key -> working" fails loudly with the one thing
// to change — wrong base URL, bad key, or model not found — not a stack trace.
func ClassifySetupProbe(result providerhealth.Result) (SetupProbeError, bool) {
	if result.Status == providerhealth.StatusPass {
		return SetupProbeError{}, false
	}

	check := result.PrimaryCheck()
	category := providerhealth.Category("")
	detail := ""
	if check != nil {
		category = check.Category
		detail = strings.TrimSpace(check.Message)
	}

	switch category {
	case providerhealth.CategoryAuth:
		return SetupProbeError{
			Class:   SetupProbeAuth,
			Message: "The provider rejected the API key. Double-check the key value (and that it is for this provider), then re-run setup.",
			Detail:  detail,
		}, true
	case providerhealth.CategoryNetwork, providerhealth.CategoryTimeout, providerhealth.CategoryConnectivity:
		return SetupProbeError{
			Class:   SetupProbeEndpoint,
			Message: "Could not reach the provider endpoint. Check the base URL (including scheme and port) and that the server is running.",
			Detail:  detail,
		}, true
	case providerhealth.CategoryRateLimit:
		return SetupProbeError{
			Class:   SetupProbeRateLimit,
			Message: "The provider is rate-limiting requests. The key is reaching it; wait a moment and re-run setup.",
			Detail:  detail,
		}, true
	case providerhealth.CategoryConfig, providerhealth.CategoryUnsupported:
		return SetupProbeError{
			Class:   SetupProbeConfig,
			Message: "The provider profile is incomplete. Set the missing field (model, base URL, or provider kind) and re-run setup.",
			Detail:  detail,
		}, true
	case providerhealth.CategoryProvider:
		// A provider-side error: a 404 / "model not found" is the common, fixable
		// case at first run, so classify those as a model problem; anything else is
		// an unclassified provider error reported verbatim.
		if isModelNotFound(check, result.Model) {
			return SetupProbeError{
				Class:   SetupProbeModel,
				Message: "The configured model was not found at the provider. Pick a model the provider serves, then re-run setup.",
				Detail:  detail,
			}, true
		}
		return SetupProbeError{Class: SetupProbeUnknown, Message: "The provider returned an error.", Detail: detail}, true
	default:
		return SetupProbeError{Class: SetupProbeUnknown, Message: "The provider probe failed.", Detail: detail}, true
	}
}

// isModelNotFound reports whether a provider-category check looks like a
// model-not-found error: a message that names the model alongside a "not
// found"/"does not exist"/"unknown" phrase, or a 404 whose message actually
// references a model lookup. A bare 404 is intentionally NOT treated as a model
// problem — a wrong base URL/path is an equally common first-run cause, and that
// is reported as the endpoint, not the model.
func isModelNotFound(check *providerhealth.Check, model string) bool {
	if check == nil {
		return false
	}
	message := strings.ToLower(check.Message)
	model = strings.ToLower(strings.TrimSpace(model))
	mentionsModel := strings.Contains(message, "model") || (model != "" && strings.Contains(message, model))
	if code, ok := statusCode(check.Details); ok && code == 404 && mentionsModel {
		return true
	}
	if strings.Contains(message, "model") && (strings.Contains(message, "not found") || strings.Contains(message, "does not exist") || strings.Contains(message, "unknown")) {
		return true
	}
	if model != "" && strings.Contains(message, model) && strings.Contains(message, "not found") {
		return true
	}
	return false
}

func statusCode(details map[string]any) (int, bool) {
	if details == nil {
		return 0, false
	}
	switch value := details["statusCode"].(type) {
	case int:
		return value, true
	case int64:
		return int(value), true
	case float64:
		return int(value), true
	default:
		return 0, false
	}
}
