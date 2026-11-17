package mcp

import (
	"context"
	"sync"
)

// launchSink carries the fact that a server's PROCESS STARTED out of the connect
// attempt, without waiting for that attempt to finish.
//
// The startup notices used to leave connectStdio only on the returned client, or
// on the returned error. Both require the attempt to return. Registration
// abandons a server that exceeds the connect timeout, so a server that started
// under the reduced write confinement and then hung in initialize or tools/list
// was recorded as skipped with nothing said about the confinement it ran under.
// The reaper that later collects the abandoned attempt runs after the serial
// commit phase has finished, so it cannot contribute without breaking the
// deterministic ordering that phase exists to provide.
//
// Publishing at Start splits the two facts apart, which is the point: launch and
// connection usability have different lifetimes. A sink that was never published
// to means Start never happened, so prepare, pipe, and Start failures stay silent
// exactly as before.
//
// IT IS ALSO AN EVENT, NOT ONLY A VALUE. A retained sink that nobody re-reads is
// still a lost disclosure: both production reporters sample once, immediately
// after registration returns, and a Start that completes after that sample had
// no way to reach them. onPublish lets a reporter subscribe; if the launch has
// already happened by the time it subscribes, it is told at once, so the fact
// reaches exactly one presentation regardless of which side won the race.
type launchSink struct {
	mu        sync.Mutex
	launched  bool
	notices   []string
	onPublish func(notices []string)
	delivered bool
}

type launchSinkKey struct{}

// withLaunchSink attaches a sink to the context handed to the client factory.
// Carried on the context rather than added to the factory signature so an
// injected or third-party factory that knows nothing about it still works, and
// simply discloses nothing.
func withLaunchSink(ctx context.Context, sink *launchSink) context.Context {
	return context.WithValue(ctx, launchSinkKey{}, sink)
}

// publishLaunch records that the process for this connect attempt has started,
// along with the enforcement notices that applied to it. Safe on a context with
// no sink, which is every caller outside registration.
func publishLaunch(ctx context.Context, notices []string) {
	sink, _ := ctx.Value(launchSinkKey{}).(*launchSink)
	if sink == nil {
		return
	}
	sink.mu.Lock()
	sink.launched = true
	sink.notices = append([]string(nil), notices...)
	deliver := sink.pendingDeliveryLocked()
	sink.mu.Unlock()
	if deliver != nil {
		deliver()
	}
}

// PublishLaunchForTest is publishLaunch for a test in another package that
// injects a client factory and needs to mark its fake process as started. It
// is the same function with the same context lookup, so a test exercises the
// real sink rather than a stand-in, and it is inert on any context that did
// not come through registration.
func PublishLaunchForTest(ctx context.Context, notices []string) {
	publishLaunch(ctx, notices)
}

// observe reports whether Start was reached and what applied to it. Read from
// the registration goroutine while the connect goroutine may still be running,
// hence the lock.
func (sink *launchSink) observe() (bool, []string) {
	if sink == nil {
		return false, nil
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.launched, append([]string(nil), sink.notices...)
}

// subscribe registers the one presentation this launch should reach. If the
// launch already happened, fn runs before subscribe returns; otherwise it runs
// from publishLaunch. Either way it runs at most once, and a second subscriber
// replaces nothing: the first delivery is the only delivery.
func (sink *launchSink) subscribe(fn func(notices []string)) {
	if sink == nil || fn == nil {
		return
	}
	sink.mu.Lock()
	if sink.onPublish == nil {
		sink.onPublish = fn
	}
	deliver := sink.pendingDeliveryLocked()
	sink.mu.Unlock()
	if deliver != nil {
		deliver()
	}
}

// pendingDeliveryLocked returns the delivery to perform, or nil, and marks it
// done. Called with mu held; the returned closure must be invoked with mu
// released, since a subscriber may itself take other locks.
func (sink *launchSink) pendingDeliveryLocked() func() {
	if !sink.launched || sink.onPublish == nil || sink.delivered {
		return nil
	}
	sink.delivered = true
	fn := sink.onPublish
	notices := append([]string(nil), sink.notices...)
	return func() { fn(notices) }
}
