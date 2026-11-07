package oauth

import (
	"context"
	"net"
	"net/http"
	"time"
)

// callbackReadHeaderTimeout bounds how long an accepted connection may take to
// send its request header. A single-use loopback callback server has no reason
// to wait longer, and without a bound a client that stalls mid-header holds the
// connection, and the server goroutine, open indefinitely. A var so the
// package tests can shorten it.
var callbackReadHeaderTimeout = 5 * time.Second

// callbackShutdownBudget is how long Close waits for in-flight requests before
// it force-closes whatever is still open.
const callbackShutdownBudget = time.Second

// CallbackServer is the lifecycle every loopback OAuth callback server needs,
// in one place.
//
// It exists because the same server was written out three times (the shared
// LoopbackListener, MCP's Login and OpenRouter's login) and a fix to one of
// them did not reach the others. http.Server.Shutdown only closes idle
// connections, so a client that has sent part of a request header is active,
// survives the shutdown deadline, and keeps its goroutine and the Serve
// goroutine alive past the end of the flow (#1028). #1051 closed that for
// LoopbackListener; the other two kept the original construction.
type CallbackServer struct {
	server *http.Server
	done   chan struct{}
}

// StartCallbackServer serves handler on listener until Close.
func StartCallbackServer(listener net.Listener, handler http.Handler) *CallbackServer {
	return startCallbackServer(listener, handler, nil)
}

// startCallbackServer is StartCallbackServer with a ConnState hook, which the
// package's own tests use to see a connection reach a given state.
func startCallbackServer(listener net.Listener, handler http.Handler, connState func(net.Conn, http.ConnState)) *CallbackServer {
	s := &CallbackServer{
		server: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: callbackReadHeaderTimeout,
			ConnState:         connState,
		},
		done: make(chan struct{}),
	}
	go func() {
		_ = s.server.Serve(listener)
		close(s.done)
	}()
	return s
}

// Close shuts the server down within callbackShutdownBudget. A connection still
// open when the budget runs out (a stalled request header, say) is force-closed
// rather than leaked, and Close returns only after the Serve goroutine has
// finished, so nothing this server started outlives the call. Idempotent.
func (s *CallbackServer) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), callbackShutdownBudget)
	defer cancel()
	if err := s.server.Shutdown(ctx); err != nil {
		_ = s.server.Close()
	}
	<-s.done
}
