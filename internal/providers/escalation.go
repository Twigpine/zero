package providers

import (
	"context"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// EscalationSwitchers builds the two mid-run model switchers a run installs when
// it has opted into escalation.
//
// ONE IMPLEMENTATION, TWO SURFACES. exec grew this first, and the interactive TUI
// needs the same behaviour rather than a similar one. The part that is easy to get
// subtly wrong is not the switch, it is the nil handling around it, and a second
// copy of that would only ever be exercised on one surface.
//
// THE NIL CONTRACTS ARE THE CONTRACT. The agent loop reassigns the provider only
// when a switcher returns a non-nil one, so a (nil, nil) return means "no swap"
// and has to leave everything as it was, including whatever the caller tracks
// through onSwitch. onSwitch therefore fires only on a real swap, and may be nil.
// An error is reported to the loop, which records a note and continues on the
// current model.
//
// The session switcher comes back nil unless the run STARTED optimized. A run
// that began on the default adapter stays on it, so escalation cannot quietly
// change the transport underneath a session.
func EscalationSwitchers(
	profile config.ProviderProfile,
	provider zeroruntime.Provider,
	newProvider func(config.ProviderProfile) (zeroruntime.Provider, error),
	onSwitch func(modelID string),
) (
	func(context.Context, string) (zeroruntime.Provider, error),
	func(context.Context, string) (zeroruntime.TurnSessionProvider, error),
) {
	if newProvider == nil {
		return nil, nil
	}
	// The escalated profile is the run's profile with the model replaced, so the
	// credential, base URL and headers travel with it. Callers pass a newProvider
	// that already applies the stored key, which is why there is no per-site key
	// handling here.
	switchTo := func(modelID string) (config.ProviderProfile, zeroruntime.Provider, error) {
		switched := profile
		switched.Model = modelID
		provider, err := newProvider(switched)
		return switched, provider, err
	}

	modelSwitcher := func(_ context.Context, modelID string) (zeroruntime.Provider, error) {
		_, switchedProvider, err := switchTo(modelID)
		if err != nil {
			return nil, err
		}
		if switchedProvider == nil {
			return nil, nil
		}
		if onSwitch != nil {
			onSwitch(modelID)
		}
		return switchedProvider, nil
	}

	turnSessions, _ := OptimizedTurnSessions(profile, provider, Options{})
	if turnSessions == nil {
		return modelSwitcher, nil
	}

	sessionSwitcher := func(_ context.Context, modelID string) (zeroruntime.TurnSessionProvider, error) {
		switchedProfile, switchedProvider, err := switchTo(modelID)
		if err != nil {
			return nil, err
		}
		if switchedProvider == nil {
			return nil, nil
		}
		if onSwitch != nil {
			onSwitch(modelID)
		}
		if optimized, ok := OptimizedTurnSessions(switchedProfile, switchedProvider, Options{}); ok {
			return optimized, nil
		}
		// Ineligible target: the default adapter, but carrying the switched
		// model's own capability projection rather than the original's.
		return DefaultTurnSessions(switchedProfile, switchedProvider, Options{}), nil
	}
	return modelSwitcher, sessionSwitcher
}
