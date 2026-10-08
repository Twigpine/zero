package providers

import (
	"context"
	"errors"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

type escalationStubProvider struct{ model string }

func (escalationStubProvider) Name() string { return "stub" }

func (escalationStubProvider) StreamCompletion(context.Context, zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	ch := make(chan zeroruntime.StreamEvent)
	close(ch)
	return ch, nil
}

// THE ESCALATED PROFILE IS THE RUN'S PROFILE WITH THE MODEL REPLACED.
//
// Everything else has to travel: the base URL, the credential and the headers.
// Building a bare profile with only the model set would escalate onto an
// unauthenticated endpoint, and the failure would look like a provider outage.
func TestEscalationSwitchersKeepTheProfileAndReplaceOnlyTheModel(t *testing.T) {
	profile := config.ProviderProfile{Name: "acme", Model: "small", BaseURL: "https://acme.test", APIKey: "sk-live"}
	var asked config.ProviderProfile
	switcher, _ := EscalationSwitchers(profile, escalationStubProvider{}, func(p config.ProviderProfile) (zeroruntime.Provider, error) {
		asked = p
		return escalationStubProvider{model: p.Model}, nil
	}, nil)
	if switcher == nil {
		t.Fatal("no model switcher was built for a run that opted into escalation")
	}
	if _, err := switcher(context.Background(), "large"); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if asked.Model != "large" {
		t.Errorf("escalated to model %q, want large", asked.Model)
	}
	if asked.BaseURL != profile.BaseURL || asked.APIKey != profile.APIKey || asked.Name != profile.Name {
		t.Errorf("the escalated profile lost the run's provider identity: %+v", asked)
	}
}

// A (nil, nil) RETURN MEANS NO SWAP, AND MUST CHANGE NOTHING.
//
// The agent loop reassigns the provider only when it gets a non-nil one, so a nil
// provider leaves the run on its current model. Anything the caller tracks
// through onSwitch has to stay in step with that, or usage gets attributed to a
// model the run never moved to.
func TestEscalationSwitchersDoNotReportASwitchThatDidNotHappen(t *testing.T) {
	switched := []string{}
	switcher, _ := EscalationSwitchers(
		config.ProviderProfile{Model: "small"}, escalationStubProvider{},
		func(config.ProviderProfile) (zeroruntime.Provider, error) { return nil, nil },
		func(modelID string) { switched = append(switched, modelID) },
	)
	provider, err := switcher(context.Background(), "large")
	if err != nil {
		t.Fatalf("a nil provider is not an error: %v", err)
	}
	if provider != nil {
		t.Errorf("provider = %v, want nil", provider)
	}
	if len(switched) != 0 {
		t.Errorf("onSwitch fired for a swap that never happened: %v", switched)
	}
}

// An error reaches the loop, which records it and stays on the current model.
func TestEscalationSwitchersReportProviderErrors(t *testing.T) {
	want := errors.New("no credential for that model")
	switched := []string{}
	switcher, _ := EscalationSwitchers(
		config.ProviderProfile{Model: "small"}, escalationStubProvider{},
		func(config.ProviderProfile) (zeroruntime.Provider, error) { return nil, want },
		func(modelID string) { switched = append(switched, modelID) },
	)
	if _, err := switcher(context.Background(), "large"); !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
	if len(switched) != 0 {
		t.Errorf("onSwitch fired for a failed switch: %v", switched)
	}
}

// And a real swap does report itself, or the caller's attribution never moves.
func TestEscalationSwitchersReportARealSwitch(t *testing.T) {
	switched := []string{}
	switcher, _ := EscalationSwitchers(
		config.ProviderProfile{Model: "small"}, escalationStubProvider{},
		func(p config.ProviderProfile) (zeroruntime.Provider, error) {
			return escalationStubProvider{model: p.Model}, nil
		},
		func(modelID string) { switched = append(switched, modelID) },
	)
	if _, err := switcher(context.Background(), "large"); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if len(switched) != 1 || switched[0] != "large" {
		t.Errorf("onSwitch recorded %v, want one switch to large", switched)
	}
}

// A RUN THAT STARTED ON THE DEFAULT ADAPTER STAYS ON IT.
//
// The session switcher is installed only when the run START is optimized, so
// escalation cannot quietly change the transport underneath a session that never
// had one.
func TestEscalationSwitchersOmitTheSessionSwitcherForAnUnoptimizedStart(t *testing.T) {
	_, sessionSwitcher := EscalationSwitchers(
		config.ProviderProfile{Name: "acme", Model: "small"}, escalationStubProvider{},
		func(p config.ProviderProfile) (zeroruntime.Provider, error) { return escalationStubProvider{}, nil },
		nil,
	)
	if sessionSwitcher != nil {
		t.Error("a run that did not start with optimized turn sessions was given a session switcher")
	}
}

// No provider factory means no escalation rather than a switcher that panics.
func TestEscalationSwitchersRequireAProviderFactory(t *testing.T) {
	switcher, sessionSwitcher := EscalationSwitchers(config.ProviderProfile{}, escalationStubProvider{}, nil, nil)
	if switcher != nil || sessionSwitcher != nil {
		t.Error("switchers were built without a provider factory to build providers with")
	}
}
