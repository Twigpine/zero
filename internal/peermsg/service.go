package peermsg

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Twigpine/zero/internal/fsutil"
)

type Handler func(InboundMessage) bool

type StatusHandler func(StatusEvent)

type HeldEvictionHandler func(string)

type HeldReleaseHandler func(InboundMessage)

const (
	peerBucketCapacity    = 30.0
	peerRefillPerSecond   = 0.5
	peerDedupWindow       = 30 * time.Second
	peerMaxSelfHops       = 10
	peerMaxChainLength    = 28
	peerMaxTrackedSenders = 256
	peerMaxHeldMessages   = 100
	peerMaxOutstanding    = 256
	peerOutstandingTTL    = 6 * time.Minute
	peerProbeWorkers      = 8
	peerProbeTimeout      = 300 * time.Millisecond
	peerInboundWorkers    = 32
	peerReceiptWorkers    = peerMaxHeldMessages
	peerReceiptTimeout    = 750 * time.Millisecond
	peerRegistryCacheTTL  = 250 * time.Millisecond
	peerFrameTimeout      = 5 * time.Second
	peerRefHexLength      = 12
	peerErrorMaxRunes     = 512
)

type inboundContextKey struct{}

type senderGuard struct {
	tokens       float64
	lastRefill   time.Time
	lastBody     string
	lastBodyAt   time.Time
	lastActivity time.Time
}

type outstandingMessage struct {
	peer      Peer
	createdAt time.Time
}

type Options struct {
	RootDir       string
	Identity      Identity
	Now           func() time.Time
	Transport     localTransport
	PID           int
	InboundPolicy InboundPolicy
}

// Service owns one live peer endpoint plus the registry used to discover other
// local Zero sessions. The transport is platform-specific, while framing,
// identity resolution, limits, and delivery policy remain shared.
type Service struct {
	mu              sync.RWMutex
	root            string
	identity        Identity
	now             func() time.Time
	transport       localTransport
	pid             int
	nonce           string
	self            Peer
	listener        net.Listener
	handler         Handler
	statusHandler   StatusHandler
	evictionHandler HeldEvictionHandler
	releaseHandler  HeldReleaseHandler
	policy          InboundPolicy
	outstanding     map[string]outstandingMessage
	held            map[string]InboundMessage
	heldOrder       []string
	guards          map[string]*senderGuard
	closed          bool
	wg              sync.WaitGroup
	done            chan struct{}
	inboundSlots    chan struct{}
	registryMu      sync.Mutex
	registryCache   []Peer
	registryModTime time.Time
	registryCached  time.Time
	registryValid   bool
}

func New(options Options) (*Service, error) {
	root := strings.TrimSpace(options.RootDir)
	if root == "" {
		var err error
		root, err = DefaultRoot()
		if err != nil {
			return nil, err
		}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("peer messaging: resolve runtime directory: %w", err)
	}
	abs, err = canonicalRuntimePath(abs)
	if err != nil {
		return nil, fmt.Errorf("peer messaging: canonicalize runtime directory: %w", err)
	}
	nonce, err := randomHex(8)
	if err != nil {
		return nil, fmt.Errorf("peer messaging: generate endpoint id: %w", err)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	transport := options.Transport
	if transport == nil {
		transport = platformTransport()
	}
	pid := options.PID
	if pid <= 0 {
		pid = os.Getpid()
	}
	return &Service{
		root:         abs,
		identity:     normalizeIdentity(options.Identity),
		now:          now,
		transport:    transport,
		pid:          pid,
		nonce:        nonce,
		policy:       normalizeInboundPolicy(options.InboundPolicy),
		outstanding:  make(map[string]outstandingMessage),
		held:         make(map[string]InboundMessage),
		guards:       make(map[string]*senderGuard),
		done:         make(chan struct{}),
		inboundSlots: make(chan struct{}, peerInboundWorkers),
	}, nil
}

// canonicalRuntimePath normalizes aliases in the existing prefix. It does not
// establish a security boundary; ensurePrivateDir validates that boundary when
// the service starts.
func canonicalRuntimePath(path string) (string, error) {
	missing := make([]string, 0, 4)
	existing := filepath.Clean(path)
	for {
		_, err := os.Lstat(existing)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", err
		}
		missing = append(missing, filepath.Base(existing))
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	for index := len(missing) - 1; index >= 0; index-- {
		resolved = filepath.Join(resolved, missing[index])
	}
	return resolved, nil
}

// WithInboundMessage carries a peer message's relay chain through the agent
// turn so a deliberate send_message reply or forward can extend it. Ordinary
// user turns have no chain and start a fresh one.
func WithInboundMessage(ctx context.Context, message InboundMessage) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	chain := append([]string(nil), message.HopChain...)
	return context.WithValue(ctx, inboundContextKey{}, chain)
}

func DefaultRoot() (string, error) {
	if runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); runtimeDir != "" {
		return filepath.Join(runtimeDir, "zero", "peers"), nil
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("peer messaging: resolve user cache directory: %w", err)
	}
	return filepath.Join(cacheDir, "zero", "peers"), nil
}

func (service *Service) Start(handler Handler) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.listener != nil {
		return nil
	}
	if service.closed {
		return errors.New("peer messaging: service is closed")
	}
	if err := ensurePrivateDir(service.root); err != nil {
		return fmt.Errorf("peer messaging: create runtime directory: %w", err)
	}
	if err := ensurePrivateDir(service.registryDir()); err != nil {
		return fmt.Errorf("peer messaging: create registry: %w", err)
	}
	endpoint, err := service.transport.Endpoint(service.root, service.nonce, service.pid)
	if err != nil {
		return err
	}
	listener, err := service.transport.Listen(endpoint)
	if err != nil {
		return fmt.Errorf("peer messaging: listen: %w", err)
	}
	now := service.now().UTC()
	if service.identity.SessionID == "" {
		service.identity.SessionID = "live-" + service.nonce
	}
	service.self = Peer{
		Identity:  service.identity,
		Endpoint:  endpoint,
		PID:       service.pid,
		StartedAt: now,
		UpdatedAt: now,
		Ref:       peerRef(endpoint),
	}
	service.listener = listener
	service.handler = handler
	if err := service.writeRecordLocked(); err != nil {
		service.listener = nil
		return errors.Join(err, listener.Close(), service.transport.Remove(endpoint))
	}
	service.wg.Add(1)
	go service.acceptLoop(listener)
	return nil
}

func (service *Service) SetStatusHandler(handler StatusHandler) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.statusHandler = handler
}

func (service *Service) SetHeldEvictionHandler(handler HeldEvictionHandler) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.evictionHandler = handler
}

func (service *Service) SetHeldReleaseHandler(handler HeldReleaseHandler) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.releaseHandler = handler
}

func (service *Service) Close() error {
	service.mu.Lock()
	if service.closed {
		service.mu.Unlock()
		return nil
	}
	service.closed = true
	close(service.done)
	listener := service.listener
	endpoint := service.self.Endpoint
	held := make([]InboundMessage, 0, len(service.held))
	for _, message := range service.held {
		held = append(held, message)
	}
	service.held = make(map[string]InboundMessage)
	service.heldOrder = nil
	service.outstanding = make(map[string]outstandingMessage)
	service.listener = nil
	service.mu.Unlock()

	service.sendStatuses(held, DeliveryExpired)

	var closeErrs []error
	if listener != nil {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			closeErrs = append(closeErrs, err)
		}
	}
	service.wg.Wait()
	if endpoint != "" {
		if err := service.transport.Remove(endpoint); err != nil && !errors.Is(err, os.ErrNotExist) {
			closeErrs = append(closeErrs, err)
		}
	}
	if err := service.removeOwnRecord(endpoint); err != nil {
		closeErrs = append(closeErrs, err)
	}
	return errors.Join(closeErrs...)
}

// ResolveHeld settles a message that was parked for local approval and sends a
// terminal receipt back to its sender. Repeated or stale decisions are ignored.
func (service *Service) ResolveHeld(ctx context.Context, messageID string, status DeliveryStatus) error {
	if status != DeliveryDelivered && status != DeliveryDenied && status != DeliveryExpired {
		return fmt.Errorf("peer messaging: invalid held-message status %q", status)
	}
	service.mu.Lock()
	message, ok := service.held[messageID]
	if ok {
		delete(service.held, messageID)
		service.removeHeldOrderLocked(messageID)
	}
	service.mu.Unlock()
	if !ok {
		return nil
	}
	return service.sendStatus(ctx, message, status)
}

func (service *Service) UpdateIdentity(identity Identity) error {
	service.mu.Lock()
	service.identity = normalizeIdentity(identity)
	if service.listener == nil {
		service.mu.Unlock()
		return nil
	}
	service.self.Identity = service.identity
	service.self.UpdatedAt = service.now().UTC()
	if err := service.writeRecordLocked(); err != nil {
		service.mu.Unlock()
		return err
	}
	released := make([]InboundMessage, 0)
	if service.policy == InboundPolicyParity && service.releaseHandler != nil {
		for _, messageID := range append([]string(nil), service.heldOrder...) {
			message, ok := service.held[messageID]
			if !ok {
				service.removeHeldOrderLocked(messageID)
				continue
			}
			status, _ := service.inboundDecision(message.From.PermissionClass, service.self.PermissionClass)
			if status != DeliveryAccepted {
				continue
			}
			delete(service.held, messageID)
			service.removeHeldOrderLocked(messageID)
			message.RequiresApproval = false
			message.HoldCause = ""
			released = append(released, message)
		}
	}
	releaseHandler := service.releaseHandler
	service.mu.Unlock()
	for _, message := range released {
		if releaseHandler != nil {
			releaseHandler(message)
		}
	}
	if len(released) > 0 {
		service.launchStatuses(released, DeliveryDelivered)
	}
	return nil
}

func (service *Service) Self() Peer {
	service.mu.RLock()
	defer service.mu.RUnlock()
	return service.self
}

func (service *Service) List(ctx context.Context) ([]Peer, error) {
	candidates, err := service.registryPeers()
	if err != nil {
		return nil, err
	}
	self := service.Self()
	type probeResult struct {
		peer      Peer
		reachable bool
	}
	jobs := make(chan Peer)
	results := make(chan probeResult, len(candidates))
	workers := min(peerProbeWorkers, len(candidates))
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for peer := range jobs {
				if peer.Endpoint == self.Endpoint || peer.SessionID == "" {
					continue
				}
				probeCtx, cancel := context.WithTimeout(ctx, peerProbeTimeout)
				conn, dialErr := service.transport.Dial(probeCtx, peer.Endpoint)
				cancel()
				if dialErr == nil {
					_ = conn.Close()
				}
				results <- probeResult{peer: peer, reachable: dialErr == nil}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, peer := range candidates {
			select {
			case jobs <- peer:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()
	peers := make([]Peer, 0, len(candidates))
	for result := range results {
		if result.reachable {
			peers = append(peers, result.peer)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("peer messaging: list sessions: %w", err)
	}
	sort.Slice(peers, func(i, j int) bool {
		if strings.EqualFold(peers[i].Name, peers[j].Name) {
			return peers[i].Ref < peers[j].Ref
		}
		return strings.ToLower(peers[i].Name) < strings.ToLower(peers[j].Name)
	})
	return peers, nil
}

func (service *Service) registryPeers() ([]Peer, error) {
	service.registryMu.Lock()
	defer service.registryMu.Unlock()

	info, statErr := os.Stat(service.registryDir())
	if errors.Is(statErr, os.ErrNotExist) {
		service.registryCache = nil
		service.registryModTime = time.Time{}
		service.registryCached = service.now()
		service.registryValid = true
		return nil, nil
	}
	if statErr != nil {
		return nil, fmt.Errorf("peer messaging: inspect registry: %w", statErr)
	}
	now := service.now()
	if service.registryValid && info.ModTime().Equal(service.registryModTime) && now.Sub(service.registryCached) < peerRegistryCacheTTL {
		return append([]Peer(nil), service.registryCache...), nil
	}
	entries, err := os.ReadDir(service.registryDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("peer messaging: read registry: %w", err)
	}
	peers := make([]Peer, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		peer, readErr := readPeerRecord(filepath.Join(service.registryDir(), entry.Name()))
		if readErr == nil && peer.Endpoint != "" && peer.SessionID != "" {
			peers = append(peers, peer)
		}
	}
	service.registryCache = append(service.registryCache[:0], peers...)
	service.registryModTime = info.ModTime()
	service.registryCached = now
	service.registryValid = true
	return append([]Peer(nil), peers...), nil
}

// registeredPeer replaces all sender-controlled identity metadata with the
// record published by the live service that owns the claimed endpoint.
func (service *Service) registeredPeer(claimed Peer) (Peer, error) {
	peers, err := service.registryPeers()
	if err != nil {
		return Peer{}, err
	}
	for _, peer := range peers {
		if peer.Endpoint == claimed.Endpoint && peer.SessionID == claimed.SessionID && peer.Ref == claimed.Ref {
			return peer, nil
		}
	}
	return Peer{}, errors.New("peer messaging: unregistered sender")
}

func (service *Service) Send(ctx context.Context, to, summary, body string) (SendResult, error) {
	body = normalizeBody(body)
	summary = normalizeSummary(summary)
	if body == "" {
		return SendResult{}, errors.New("peer messaging: message must not be empty")
	}
	if len(body) > maxMessageBytes {
		return SendResult{}, fmt.Errorf("peer messaging: message exceeds %d bytes", maxMessageBytes)
	}
	if summary == "" {
		return SendResult{}, errors.New("peer messaging: summary is required")
	}
	peers, err := service.List(ctx)
	if err != nil {
		return SendResult{}, err
	}
	peer, err := resolvePeer(peers, to)
	if err != nil {
		return SendResult{}, err
	}
	id, err := randomHex(16)
	if err != nil {
		return SendResult{}, fmt.Errorf("peer messaging: generate message id: %w", err)
	}
	self := service.Self()
	hopChain := inboundHopChain(ctx)
	if len(hopChain) == 0 || hopChain[len(hopChain)-1] != self.Ref {
		hopChain = append(hopChain, self.Ref)
	}
	if err := validateHopChain(hopChain); err != nil {
		return SendResult{}, err
	}
	frame := sendFrame{
		Version:  ProtocolVersion,
		Type:     "message",
		ID:       id,
		From:     self,
		To:       peer.SessionID,
		Summary:  summary,
		Body:     body,
		HopChain: hopChain,
	}
	service.mu.Lock()
	service.pruneOutstandingLocked(service.now())
	service.outstanding[id] = outstandingMessage{peer: peer, createdAt: service.now()}
	service.mu.Unlock()
	keepOutstanding := false
	defer func() {
		if keepOutstanding {
			return
		}
		service.mu.Lock()
		delete(service.outstanding, id)
		service.mu.Unlock()
	}()
	conn, err := service.transport.Dial(ctx, peer.Endpoint)
	if err != nil {
		return SendResult{}, fmt.Errorf("peer messaging: connect to %s: %w", displayPeer(peer), err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadlineFromContext(ctx, peerFrameTimeout))
	if err := json.NewEncoder(conn).Encode(frame); err != nil {
		return SendResult{}, fmt.Errorf("peer messaging: send to %s: %w", displayPeer(peer), err)
	}
	var response responseFrame
	if err := decodeFrame(conn, &response); err != nil {
		return SendResult{}, fmt.Errorf("peer messaging: receive delivery status from %s: %w", displayPeer(peer), err)
	}
	if response.Version != ProtocolVersion || response.Type != "delivery" || response.ID != id {
		return SendResult{}, errors.New("peer messaging: invalid delivery response")
	}
	if response.Error != "" {
		return SendResult{}, fmt.Errorf("peer messaging: receiver rejected message: %s", normalizeRemoteError(response.Error))
	}
	if response.Status != DeliveryAccepted && response.Status != DeliveryHeld && response.Status != DeliveryRefused {
		return SendResult{}, errors.New("peer messaging: invalid delivery status")
	}
	keepOutstanding = response.Status == DeliveryHeld
	return SendResult{MessageID: id, Peer: peer, Status: response.Status}, nil
}

func (service *Service) acceptLoop(listener net.Listener) {
	defer service.wg.Done()
	var retryDelay time.Duration
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			service.mu.RLock()
			closed := service.closed
			service.mu.RUnlock()
			if closed {
				return
			}
			if retryDelay == 0 {
				retryDelay = 5 * time.Millisecond
			} else {
				retryDelay *= 2
			}
			retryDelay = min(retryDelay, time.Second)
			timer := time.NewTimer(retryDelay)
			select {
			case <-timer.C:
			case <-service.done:
				if !timer.Stop() {
					<-timer.C
				}
				return
			}
			continue
		}
		retryDelay = 0
		select {
		case service.inboundSlots <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		service.wg.Add(1)
		go func() {
			defer service.wg.Done()
			defer func() { <-service.inboundSlots }()
			defer conn.Close()
			service.handleConn(conn)
		}()
	}
}

func (service *Service) handleConn(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(peerFrameTimeout))
	var frame sendFrame
	if err := decodeFrame(conn, &frame); err != nil {
		if !errors.Is(err, io.EOF) {
			_ = json.NewEncoder(conn).Encode(responseFrame{Version: ProtocolVersion, Type: "delivery", Status: DeliveryRefused, Error: "invalid peer message"})
		}
		return
	}
	if frame.Type == "status" {
		service.handleStatusFrame(frame)
		return
	}
	response := responseFrame{Version: ProtocolVersion, Type: "delivery", ID: frame.ID, Status: DeliveryRefused}
	if frame.Version != ProtocolVersion || frame.Type != "message" || frame.ID == "" ||
		frame.From.SessionID == "" || frame.From.Endpoint == "" || !validPeerRef(frame.From.Endpoint, frame.From.Ref) {
		response.Error = "peer messaging: invalid message envelope"
		_ = json.NewEncoder(conn).Encode(response)
		return
	}
	trustedSender, err := service.registeredPeer(frame.From)
	if err != nil {
		response.Error = "peer messaging: sender identity is not registered"
		_ = json.NewEncoder(conn).Encode(response)
		return
	}
	frame.From = trustedSender
	if len(frame.Body) == 0 || len(frame.Body) > maxMessageBytes {
		response.Error = "peer messaging: invalid message size"
		_ = json.NewEncoder(conn).Encode(response)
		return
	}
	body := normalizeBody(frame.Body)
	if body == "" {
		response.Error = "peer messaging: invalid message content"
		_ = json.NewEncoder(conn).Encode(response)
		return
	}
	service.mu.RLock()
	self := service.self
	handler := service.handler
	service.mu.RUnlock()
	if self.SessionID == "" || frame.To != self.SessionID {
		response.Error = "peer messaging: target session is no longer active"
		_ = json.NewEncoder(conn).Encode(response)
		return
	}
	if len(frame.HopChain) == 0 {
		frame.HopChain = []string{frame.From.Ref}
	}
	if err := validateHopChain(frame.HopChain); err != nil || frame.HopChain[len(frame.HopChain)-1] != frame.From.Ref {
		response.Error = "peer messaging: invalid relay chain"
		_ = json.NewEncoder(conn).Encode(response)
		return
	}
	if reason := service.admitMessage(frame.From, body, frame.HopChain, self.Ref); reason != "" {
		response.Error = "peer messaging: message dropped: " + reason
		_ = json.NewEncoder(conn).Encode(response)
		return
	}
	if handler == nil {
		response.Status = DeliveryRefused
		response.Error = "peer messaging: receiving session is unavailable"
		_ = json.NewEncoder(conn).Encode(response)
		return
	}
	status, holdCause := service.inboundDecision(frame.From.PermissionClass, self.PermissionClass)
	response.Status = status
	if status == DeliveryRefused {
		_ = json.NewEncoder(conn).Encode(response)
		return
	}
	message := InboundMessage{
		ID:               frame.ID,
		From:             frame.From,
		Body:             body,
		Summary:          normalizeSummary(frame.Summary),
		ReceivedAt:       service.now().UTC(),
		RequiresApproval: status == DeliveryHeld,
		HoldCause:        holdCause,
		HopChain:         append([]string(nil), frame.HopChain...),
	}
	if status == DeliveryHeld {
		var evicted InboundMessage
		var evictionHandler HeldEvictionHandler
		service.mu.Lock()
		if len(service.held) >= peerMaxHeldMessages {
			evicted = service.popOldestHeldLocked()
			evictionHandler = service.evictionHandler
		}
		service.held[message.ID] = message
		service.heldOrder = append(service.heldOrder, message.ID)
		service.mu.Unlock()
		if evicted.ID != "" {
			if evictionHandler != nil {
				evictionHandler(evicted.ID)
			}
			service.launchStatuses([]InboundMessage{evicted}, DeliveryExpired)
		}
	}
	_ = conn.SetDeadline(time.Now().Add(peerFrameTimeout))
	if !handler(message) {
		if status == DeliveryHeld {
			service.mu.Lock()
			delete(service.held, message.ID)
			service.removeHeldOrderLocked(message.ID)
			service.mu.Unlock()
		}
		response.Status = DeliveryRefused
		response.Error = "peer messaging: message dropped: receiver queue is full or unavailable"
		_ = json.NewEncoder(conn).Encode(response)
		return
	}
	_ = json.NewEncoder(conn).Encode(response)
}

func (service *Service) handleStatusFrame(frame sendFrame) {
	if frame.Version != ProtocolVersion || frame.OrigID == "" || frame.From.SessionID == "" ||
		frame.From.Endpoint == "" || !validPeerRef(frame.From.Endpoint, frame.From.Ref) || !terminalDeliveryStatus(frame.Status) {
		return
	}
	trustedSender, err := service.registeredPeer(frame.From)
	if err != nil {
		return
	}
	frame.From = trustedSender
	service.mu.Lock()
	self := service.self
	outstanding, ok := service.outstanding[frame.OrigID]
	if !ok || frame.To != self.SessionID || outstanding.peer.Endpoint != frame.From.Endpoint || outstanding.peer.SessionID != frame.From.SessionID {
		service.mu.Unlock()
		return
	}
	delete(service.outstanding, frame.OrigID)
	handler := service.statusHandler
	service.mu.Unlock()
	if handler != nil {
		handler(StatusEvent{MessageID: frame.OrigID, Peer: outstanding.peer, Status: frame.Status})
	}
}

func (service *Service) sendStatus(ctx context.Context, message InboundMessage, status DeliveryStatus) error {
	if !terminalDeliveryStatus(status) {
		return fmt.Errorf("peer messaging: invalid delivery status %q", status)
	}
	target := message.From
	if target.Endpoint == "" || target.SessionID == "" || !validPeerRef(target.Endpoint, target.Ref) {
		return errors.New("peer messaging: original sender identity is invalid")
	}
	self := service.Self()
	frame := sendFrame{
		Version: ProtocolVersion,
		Type:    "status",
		From:    self,
		To:      target.SessionID,
		OrigID:  message.ID,
		Status:  status,
	}
	conn, err := service.transport.Dial(ctx, target.Endpoint)
	if err != nil {
		return fmt.Errorf("peer messaging: send status to %s: %w", displayPeer(target), err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadlineFromContext(ctx, peerFrameTimeout))
	if err := json.NewEncoder(conn).Encode(frame); err != nil {
		return fmt.Errorf("peer messaging: send status to %s: %w", displayPeer(target), err)
	}
	return nil
}

func (service *Service) sendStatuses(messages []InboundMessage, status DeliveryStatus) {
	jobs := make(chan InboundMessage)
	workers := min(peerReceiptWorkers, len(messages))
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for message := range jobs {
				ctx, cancel := context.WithTimeout(context.Background(), peerReceiptTimeout)
				_ = service.sendStatus(ctx, message, status)
				cancel()
			}
		}()
	}
	for _, message := range messages {
		jobs <- message
	}
	close(jobs)
	wg.Wait()
}

func deadlineFromContext(ctx context.Context, maximum time.Duration) time.Time {
	deadline := time.Now().Add(maximum)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		return contextDeadline
	}
	return deadline
}

func (service *Service) launchStatuses(messages []InboundMessage, status DeliveryStatus) {
	service.mu.Lock()
	if service.closed {
		service.mu.Unlock()
		return
	}
	service.wg.Add(1)
	service.mu.Unlock()
	go func() {
		defer service.wg.Done()
		service.sendStatuses(messages, status)
	}()
}

func (service *Service) pruneOutstandingLocked(now time.Time) {
	for id, pending := range service.outstanding {
		if now.Sub(pending.createdAt) >= peerOutstandingTTL {
			delete(service.outstanding, id)
		}
	}
	for len(service.outstanding) >= peerMaxOutstanding {
		var oldestID string
		var oldest time.Time
		for id, pending := range service.outstanding {
			if oldestID == "" || pending.createdAt.Before(oldest) {
				oldestID, oldest = id, pending.createdAt
			}
		}
		delete(service.outstanding, oldestID)
	}
}

func (service *Service) inboundDecision(sender, receiver PermissionClass) (DeliveryStatus, HoldCause) {
	switch service.policy {
	case InboundPolicyAccept:
		return DeliveryAccepted, ""
	case InboundPolicyHold:
		return DeliveryHeld, HoldCauseExplicit
	case InboundPolicyRefuse:
		return DeliveryRefused, ""
	default:
		if sender == "" {
			if receiver == PermissionBypass {
				return DeliveryHeld, HoldCauseModeUnknown
			}
			return DeliveryAccepted, ""
		}
		if sender == receiver {
			return DeliveryAccepted, ""
		}
		return DeliveryHeld, HoldCauseModeMismatch
	}
}

func (service *Service) admitMessage(sender Peer, body string, chain []string, selfRef string) string {
	if len(chain) > peerMaxChainLength {
		return "relay chain is too long"
	}
	selfHops := 0
	for _, ref := range chain {
		if ref == selfRef {
			selfHops++
		}
	}
	if selfHops >= peerMaxSelfHops {
		return "peer messaging loop detected"
	}
	now := service.now()
	key := sender.Endpoint
	service.mu.Lock()
	defer service.mu.Unlock()
	guard := service.guards[key]
	if guard == nil {
		if len(service.guards) >= peerMaxTrackedSenders {
			var oldestKey string
			var oldest time.Time
			for candidate, state := range service.guards {
				if oldestKey == "" || state.lastActivity.Before(oldest) {
					oldestKey, oldest = candidate, state.lastActivity
				}
			}
			delete(service.guards, oldestKey)
		}
		guard = &senderGuard{tokens: peerBucketCapacity, lastRefill: now}
		service.guards[key] = guard
	}
	elapsed := now.Sub(guard.lastRefill).Seconds()
	if elapsed > 0 {
		guard.tokens = min(peerBucketCapacity, guard.tokens+elapsed*peerRefillPerSecond)
		guard.lastRefill = now
	}
	if guard.tokens < 1 {
		return "sender exceeded the peer message rate limit"
	}
	guard.tokens--
	if guard.lastBody == body && now.Sub(guard.lastBodyAt) < peerDedupWindow {
		return "duplicate of a recent message from this sender"
	}
	guard.lastActivity = now
	guard.lastBody = body
	guard.lastBodyAt = now
	return ""
}

func (service *Service) removeHeldOrderLocked(messageID string) {
	for index, candidate := range service.heldOrder {
		if candidate == messageID {
			service.heldOrder = append(service.heldOrder[:index], service.heldOrder[index+1:]...)
			return
		}
	}
}

func (service *Service) popOldestHeldLocked() InboundMessage {
	for len(service.heldOrder) > 0 {
		messageID := service.heldOrder[0]
		service.heldOrder = service.heldOrder[1:]
		if message, ok := service.held[messageID]; ok {
			delete(service.held, messageID)
			return message
		}
	}
	var oldestID string
	var oldest InboundMessage
	for messageID, message := range service.held {
		if oldestID == "" || message.ReceivedAt.Before(oldest.ReceivedAt) {
			oldestID, oldest = messageID, message
		}
	}
	if oldestID != "" {
		delete(service.held, oldestID)
	}
	return oldest
}

func inboundHopChain(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	chain, _ := ctx.Value(inboundContextKey{}).([]string)
	return append([]string(nil), chain...)
}

func validateHopChain(chain []string) error {
	if len(chain) == 0 || len(chain) > peerMaxChainLength {
		return errors.New("peer messaging: invalid relay chain")
	}
	for _, ref := range chain {
		if len(ref) < 8 || len(ref) > peerRefHexLength || len(ref)%2 != 0 {
			return errors.New("peer messaging: invalid relay chain")
		}
		if _, err := hex.DecodeString(ref); err != nil {
			return errors.New("peer messaging: invalid relay chain")
		}
	}
	return nil
}

func terminalDeliveryStatus(status DeliveryStatus) bool {
	return status == DeliveryDelivered || status == DeliveryDenied || status == DeliveryExpired
}

func normalizeInboundPolicy(policy InboundPolicy) InboundPolicy {
	switch policy {
	case InboundPolicyAccept, InboundPolicyHold, InboundPolicyRefuse:
		return policy
	default:
		return InboundPolicyParity
	}
}

func decodeFrame(reader io.Reader, target any) error {
	buffered := bufio.NewReader(io.LimitReader(reader, maxFrameBytes+1))
	line, err := buffered.ReadBytes('\n')
	if err != nil {
		return err
	}
	if len(line) > maxFrameBytes {
		return errors.New("peer messaging: frame is too large")
	}
	if err := json.Unmarshal(line, target); err != nil {
		return fmt.Errorf("peer messaging: decode frame: %w", err)
	}
	return nil
}

func (service *Service) registryDir() string { return filepath.Join(service.root, "registry") }

func (service *Service) recordPath() string {
	return filepath.Join(service.registryDir(), fmt.Sprintf("%d-%s.json", service.pid, service.nonce))
}

func (service *Service) writeRecordLocked() error {
	data, err := json.Marshal(service.self)
	if err != nil {
		return fmt.Errorf("peer messaging: encode registry record: %w", err)
	}
	path := service.recordPath()
	tmp, err := os.CreateTemp(service.registryDir(), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("peer messaging: create registry temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := fsutil.RenameWithRetry(tmpPath, path, nil); err != nil {
		return fmt.Errorf("peer messaging: publish registry record: %w", err)
	}
	return nil
}

func readPeerRecord(path string) (Peer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Peer{}, err
	}
	var peer Peer
	if err := json.Unmarshal(data, &peer); err != nil {
		return Peer{}, err
	}
	if peer.PID <= 0 || peer.Endpoint == "" || !validPeerRef(peer.Endpoint, peer.Ref) {
		return Peer{}, errors.New("peer messaging: invalid registry record")
	}
	peer.Identity = normalizeIdentity(peer.Identity)
	return peer, nil
}

func (service *Service) removeOwnRecord(endpoint string) error {
	path := service.recordPath()
	peer, err := readPeerRecord(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("peer messaging: verify registry record before removal: %w", err)
	}
	if peer.Endpoint != endpoint {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("peer messaging: remove registry record: %w", err)
	}
	return nil
}

func resolvePeer(peers []Peer, target string) (Peer, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return Peer{}, errors.New("peer messaging: recipient must not be empty")
	}
	for _, peer := range peers {
		if peer.SessionID == target {
			return peer, nil
		}
	}
	if name, ref, ok := splitPeerTarget(target); ok {
		for _, peer := range peers {
			peerName := strings.TrimSpace(peer.Name)
			if peerName == "" {
				peerName = "Zero session"
			}
			if strings.EqualFold(peer.Ref, ref) && strings.EqualFold(peerName, name) {
				return peer, nil
			}
		}
		return Peer{}, fmt.Errorf("peer messaging: no reachable session named %q", target)
	}
	matches := make([]Peer, 0, 2)
	for _, peer := range peers {
		if strings.EqualFold(peer.Name, target) {
			matches = append(matches, peer)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return Peer{}, fmt.Errorf("peer messaging: %q is ambiguous; use name [ref] from list_sessions", target)
	}
	return Peer{}, fmt.Errorf("peer messaging: no reachable session named %q", target)
}

func splitPeerTarget(target string) (name, ref string, ok bool) {
	open := strings.LastIndex(target, " [")
	if open <= 0 || !strings.HasSuffix(target, "]") {
		return "", "", false
	}
	name = strings.TrimSpace(target[:open])
	ref = target[open+2 : len(target)-1]
	if name == "" || len(ref) < 8 || len(ref) > peerRefHexLength || len(ref)%2 != 0 {
		return "", "", false
	}
	if _, err := hex.DecodeString(ref); err != nil {
		return "", "", false
	}
	return name, ref, true
}

func displayPeer(peer Peer) string {
	name := strings.TrimSpace(peer.Name)
	if name == "" {
		name = "Zero session"
	}
	return fmt.Sprintf("%s [%s]", name, peer.Ref)
}

func normalizeIdentity(identity Identity) Identity {
	identity.SessionID = truncateRunes(strings.TrimSpace(sanitizePlainText(identity.SessionID, false)), 256)
	identity.Name = truncateRunes(strings.TrimSpace(sanitizePlainText(identity.Name, false)), 80)
	if strings.ContainsAny(identity.Name, "[]") {
		identity.Name = ""
	}
	identity.Cwd = truncateRunes(strings.TrimSpace(sanitizePlainText(identity.Cwd, false)), 4096)
	switch identity.PermissionClass {
	case PermissionPrompting, PermissionBypass:
	default:
		identity.PermissionClass = ""
	}
	return identity
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func normalizeSummary(summary string) string {
	summary = strings.Split(summary, "\n")[0]
	summary = strings.TrimSpace(sanitizePlainText(summary, false))
	runes := []rune(summary)
	if len(runes) > 200 {
		summary = string(runes[:200])
	}
	return summary
}

func normalizeBody(body string) string {
	return strings.TrimSpace(sanitizePlainText(body, true))
}

func normalizeRemoteError(message string) string {
	message = truncateRunes(strings.TrimSpace(sanitizePlainText(message, false)), peerErrorMaxRunes)
	if message == "" {
		return "receiver rejected the message"
	}
	return message
}

func sanitizePlainText(value string, multiline bool) string {
	return strings.Map(func(char rune) rune {
		if multiline && (char == '\n' || char == '\t') {
			return char
		}
		if unicode.IsControl(char) {
			return -1
		}
		return char
	}, value)
}

func peerRef(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(sum[:peerRefHexLength/2])
}

func validPeerRef(endpoint, ref string) bool {
	if ref == peerRef(endpoint) {
		return true
	}
	// Accept references from a process that was already running during an
	// in-place upgrade. New sessions always publish the wider reference.
	sum := sha256.Sum256([]byte(endpoint))
	return ref == hex.EncodeToString(sum[:4])
}

func randomHex(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
