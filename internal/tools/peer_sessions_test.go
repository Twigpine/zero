package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/peermsg"
)

type fakePeerSessionService struct {
	peers   []peermsg.Peer
	sentTo  string
	summary string
	body    string
	result  peermsg.SendResult
	err     error
}

func (service *fakePeerSessionService) List(context.Context) ([]peermsg.Peer, error) {
	return service.peers, service.err
}

func (service *fakePeerSessionService) Send(_ context.Context, to, summary, body string) (peermsg.SendResult, error) {
	service.sentTo = to
	service.summary = summary
	service.body = body
	return service.result, service.err
}

func TestListSessionsToolFormatsAddressWithoutTransportDetails(t *testing.T) {
	service := &fakePeerSessionService{peers: []peermsg.Peer{{
		Identity: peermsg.Identity{SessionID: "session-1", Name: "reviewer", Cwd: "/work"},
		Endpoint: "private-endpoint",
		PID:      123,
		Ref:      "a1b2c3d4",
	}}}
	toolset := NewPeerSessionTools(service)
	result := toolset[0].Run(context.Background(), map[string]any{})
	if result.Status != StatusOK || !strings.Contains(result.Output, "reviewer [a1b2c3d4]") {
		t.Fatalf("result = %#v", result)
	}
	if strings.Contains(result.Output, "private-endpoint") || strings.Contains(result.Output, "123") {
		t.Fatalf("transport internals leaked: %q", result.Output)
	}
}

func TestSendMessageToolPassesPlainTextEnvelope(t *testing.T) {
	service := &fakePeerSessionService{result: peermsg.SendResult{
		MessageID: "m1",
		Status:    peermsg.DeliveryHeld,
		Peer:      peermsg.Peer{Identity: peermsg.Identity{SessionID: "s1", Name: "reviewer"}, Ref: "abcd1234"},
	}}
	toolset := NewPeerSessionTools(service)
	result := toolset[1].Run(context.Background(), map[string]any{
		"to": "reviewer", "summary": "review request", "message": "Please inspect this.",
	})
	if result.Status != StatusOK || result.Meta["status"] != "held" {
		t.Fatalf("result = %#v", result)
	}
	if service.sentTo != "reviewer" || service.summary != "review request" || service.body != "Please inspect this." {
		t.Fatalf("sent = %q %q %q", service.sentTo, service.summary, service.body)
	}
}

func TestPeerReplyToolIsEagerOnlyForPeerAwareTurns(t *testing.T) {
	service := &fakePeerSessionService{}
	ordinary := NewPeerSessionTools(service)
	if len(ordinary) != 2 || !IsDeferred(ordinary[1]) {
		t.Fatalf("ordinary send_message should remain deferred: %#v", ordinary)
	}
	reply := NewPeerReplyTool(service)
	if reply == nil || IsDeferred(reply) || reply.Name() != "send_message" {
		t.Fatalf("peer reply tool should be eager: %#v", reply)
	}
}

func TestPeerSessionToolsSurfaceServiceErrors(t *testing.T) {
	service := &fakePeerSessionService{err: errors.New("service unavailable")}
	toolset := NewPeerSessionTools(service)
	if result := toolset[0].Run(context.Background(), map[string]any{}); result.Status != StatusError || !strings.Contains(result.Output, "service unavailable") {
		t.Fatalf("list result = %#v", result)
	}
	if result := toolset[1].Run(context.Background(), map[string]any{"to": "peer", "summary": "test", "message": "hello"}); result.Status != StatusError || !strings.Contains(result.Output, "service unavailable") {
		t.Fatalf("send result = %#v", result)
	}
}

func TestSendMessageToolRejectsMissingAndInvalidArguments(t *testing.T) {
	tool := NewPeerSessionTools(&fakePeerSessionService{})[1]
	for _, args := range []map[string]any{
		{},
		{"to": 42, "summary": "test", "message": "hello"},
		{"to": "peer", "summary": true, "message": "hello"},
		{"to": "peer", "summary": "test", "message": []string{"hello"}},
	} {
		if result := tool.Run(context.Background(), args); result.Status != StatusError {
			t.Fatalf("args %#v result = %#v", args, result)
		}
	}
}
