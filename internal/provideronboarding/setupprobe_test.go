package provideronboarding

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/providerhealth"
)

func TestClassifySetupProbeSuccess(t *testing.T) {
	probeErr, ok := ClassifySetupProbe(providerhealth.Result{
		Status: providerhealth.StatusPass,
		Checks: []providerhealth.Check{
			{ID: "provider.connectivity", Status: providerhealth.StatusPass, Message: "reachable"},
		},
	})
	if ok {
		t.Fatalf("a passing probe must not produce an error: %#v", probeErr)
	}
}

func TestClassifySetupProbeAuthFailure(t *testing.T) {
	probeErr, ok := ClassifySetupProbe(providerhealth.Result{
		Status: providerhealth.StatusFail,
		Checks: []providerhealth.Check{
			{ID: "provider.connectivity", Status: providerhealth.StatusFail, Category: providerhealth.CategoryAuth, Message: "Provider endpoint returned 401: invalid api key"},
		},
	})
	if !ok {
		t.Fatalf("a failing auth probe must produce an error")
	}
	if probeErr.Class != SetupProbeAuth {
		t.Fatalf("class = %q, want %q", probeErr.Class, SetupProbeAuth)
	}
	if !strings.Contains(strings.ToLower(probeErr.Message), "api key") {
		t.Fatalf("auth message should point at the API key, got %q", probeErr.Message)
	}
	if probeErr.Error() == "" {
		t.Fatalf("error string must not be empty")
	}
}

func TestClassifySetupProbeModelNotFound(t *testing.T) {
	probeErr, ok := ClassifySetupProbe(providerhealth.Result{
		Status: providerhealth.StatusFail,
		Model:  "missing-model",
		Checks: []providerhealth.Check{
			{ID: "provider.connectivity", Status: providerhealth.StatusFail, Category: providerhealth.CategoryProvider, Message: "Provider endpoint returned 404: model not found", Details: map[string]any{"statusCode": 404}},
		},
	})
	if !ok {
		t.Fatalf("a model-not-found probe must produce an error")
	}
	if probeErr.Class != SetupProbeModel {
		t.Fatalf("class = %q, want %q (a 404 / model-not-found should classify as model)", probeErr.Class, SetupProbeModel)
	}
	if !strings.Contains(strings.ToLower(probeErr.Message), "model") {
		t.Fatalf("model message should mention the model, got %q", probeErr.Message)
	}
}

func TestClassifySetupProbeBadBaseURL404IsNotModel(t *testing.T) {
	// A 404 from a wrong base URL/path is a common first-run mistake. It must not be
	// classified as a missing model — that would send the user to change the model
	// when the real fix is the endpoint. With no model-lookup context in the
	// message, a provider-category 404 stays an unclassified provider error.
	probeErr, ok := ClassifySetupProbe(providerhealth.Result{
		Status: providerhealth.StatusFail,
		Model:  "gpt-4.1",
		Checks: []providerhealth.Check{
			{ID: "provider.connectivity", Status: providerhealth.StatusFail, Category: providerhealth.CategoryProvider, Message: "Provider endpoint returned 404: 404 page not found", Details: map[string]any{"statusCode": 404}},
		},
	})
	if !ok {
		t.Fatalf("a failing provider probe must produce an error")
	}
	if probeErr.Class == SetupProbeModel {
		t.Fatalf("a bad-base-URL 404 must not classify as a missing model, got %q", probeErr.Class)
	}
}

func TestClassifySetupProbeModelNotFound404WithModelContext(t *testing.T) {
	// A 404 whose message names a model lookup failure stays classified as a model
	// problem.
	probeErr, ok := ClassifySetupProbe(providerhealth.Result{
		Status: providerhealth.StatusFail,
		Model:  "gpt-4.1",
		Checks: []providerhealth.Check{
			{ID: "provider.connectivity", Status: providerhealth.StatusFail, Category: providerhealth.CategoryProvider, Message: "Provider endpoint returned 404: The model `gpt-4.1` does not exist", Details: map[string]any{"statusCode": 404}},
		},
	})
	if !ok {
		t.Fatalf("a failing provider probe must produce an error")
	}
	if probeErr.Class != SetupProbeModel {
		t.Fatalf("a model-context 404 must classify as model, got %q", probeErr.Class)
	}
}

func TestClassifySetupProbeNetworkFailure(t *testing.T) {
	cases := []struct {
		name     string
		category providerhealth.Category
	}{
		{"network", providerhealth.CategoryNetwork},
		{"timeout", providerhealth.CategoryTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probeErr, ok := ClassifySetupProbe(providerhealth.Result{
				Status: providerhealth.StatusFail,
				Checks: []providerhealth.Check{
					{ID: "provider.connectivity", Status: providerhealth.StatusFail, Category: tc.category, Message: "could not connect"},
				},
			})
			if !ok {
				t.Fatalf("a %s probe must produce an error", tc.name)
			}
			if probeErr.Class != SetupProbeEndpoint {
				t.Fatalf("class = %q, want %q", probeErr.Class, SetupProbeEndpoint)
			}
			if !strings.Contains(strings.ToLower(probeErr.Message), "base url") {
				t.Fatalf("endpoint message should point at the base URL, got %q", probeErr.Message)
			}
		})
	}
}

func TestClassifySetupProbeConfigFailure(t *testing.T) {
	probeErr, ok := ClassifySetupProbe(providerhealth.Result{
		Status: providerhealth.StatusFail,
		Checks: []providerhealth.Check{
			{ID: "provider.config", Status: providerhealth.StatusFail, Category: providerhealth.CategoryConfig, Message: "Provider openai requires model."},
		},
	})
	if !ok {
		t.Fatalf("a config failure must produce an error")
	}
	if probeErr.Class != SetupProbeConfig {
		t.Fatalf("class = %q, want %q", probeErr.Class, SetupProbeConfig)
	}
}

func TestClassifySetupProbeRateLimit(t *testing.T) {
	probeErr, ok := ClassifySetupProbe(providerhealth.Result{
		Status: providerhealth.StatusWarn,
		Checks: []providerhealth.Check{
			{ID: "provider.connectivity", Status: providerhealth.StatusWarn, Category: providerhealth.CategoryRateLimit, Message: "Provider endpoint returned 429"},
		},
	})
	if !ok {
		t.Fatalf("a rate-limit warning is a soft failure that must still surface a classified message")
	}
	if probeErr.Class != SetupProbeRateLimit {
		t.Fatalf("class = %q, want %q", probeErr.Class, SetupProbeRateLimit)
	}
}

func TestClassifySetupProbeNeverPanicsOnEmptyResult(t *testing.T) {
	// The headline guarantee: a degenerate / empty probe result must classify to a
	// stable error class, never panic and never dereference a nil check.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ClassifySetupProbe panicked on an empty result: %v", r)
		}
	}()
	probeErr, ok := ClassifySetupProbe(providerhealth.Result{Status: providerhealth.StatusFail})
	if !ok {
		t.Fatalf("a failing-but-empty result must still produce an error")
	}
	if probeErr.Class != SetupProbeUnknown {
		t.Fatalf("class = %q, want %q", probeErr.Class, SetupProbeUnknown)
	}
}
