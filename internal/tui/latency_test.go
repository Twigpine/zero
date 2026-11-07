package tui

import (
	"context"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/sessions"
)

func TestAvgTurnLatencyText(t *testing.T) {
	m := newModel(context.Background(), Options{})
	if got := m.avgTurnLatencyText(); got != "n/a" {
		t.Fatalf("empty latency = %q, want n/a", got)
	}
	m.turnLatencySum = 9 * time.Second
	m.turnLatencyCount = 2
	if got := m.avgTurnLatencyText(); got != "4.5s avg (2 turns)" {
		t.Fatalf("avgTurnLatencyText = %q, want \"4.5s avg (2 turns)\"", got)
	}
	// With TTFT recorded, the line also reports time-to-first-token.
	m.turnTTFTSum = 3 * time.Second
	m.turnTTFTCount = 2
	if got := m.avgTurnLatencyText(); got != "4.5s avg (1.5s to first token, 2 turns)" {
		t.Fatalf("avgTurnLatencyText with ttft = %q, want \"4.5s avg (1.5s to first token, 2 turns)\"", got)
	}
	// /new must reset the rolling latency + ttft so a fresh session starts from zero.
	m.activeSession = sessions.Metadata{SessionID: "x"}
	next := m.startNewSession()
	if next.turnLatencyCount != 0 || next.turnLatencySum != 0 || next.turnTTFTCount != 0 || next.turnTTFTSum != 0 {
		t.Fatalf("startNewSession must reset latency+ttft, got latency=%d/%v ttft=%d/%v", next.turnLatencyCount, next.turnLatencySum, next.turnTTFTCount, next.turnTTFTSum)
	}
}
