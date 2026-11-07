package usage

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/Twigpine/zero/internal/modelregistry"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// usageEventPayload mirrors the persisted EventUsage payload written by the exec
// runtime. Prompt/completion/total are always stored; the cache and reasoning
// breakdown is stored when present (omitempty) so cost reconstruction matches the
// live tracker instead of over-pricing cache-heavy or reasoning-heavy turns.
// Older events without those fields decode to zero and price exactly as before.
// Model is persisted only on escalation runs (the model in force can change
// mid-run only under --allow-escalation); when absent, cost is reconstructed from
// the session's Metadata.ModelID and is a labeled estimate.
type usageEventPayload struct {
	PromptTokens      int    `json:"promptTokens"`
	CompletionTokens  int    `json:"completionTokens"`
	TotalTokens       int    `json:"totalTokens"`
	CachedInputTokens int    `json:"cachedInputTokens,omitempty"`
	CacheWriteTokens  int    `json:"cacheWriteTokens,omitempty"`
	ReasoningTokens   int    `json:"reasoningTokens,omitempty"`
	Model             string `json:"model,omitempty"`
}

// EventUsagePayload builds the persisted EventUsage payload for a usage record.
// It is the single writer paired with usageEventPayload (the reader), so the JSON
// keys can never drift. Cache and reasoning counts are written only when non-zero,
// keeping payloads compact and older readers unaffected; BuildReport reads them
// back to price a turn exactly (cache discount + cache-write premium + reasoning)
// rather than estimating from prompt/completion alone. Callers add "model"
// afterward on escalation runs.
func EventUsagePayload(u zeroruntime.Usage) map[string]any {
	payload := map[string]any{
		"promptTokens":     u.EffectiveInputTokens(),
		"completionTokens": u.EffectiveOutputTokens(),
		"totalTokens":      u.TotalTokens(),
	}
	if u.CachedInputTokens > 0 {
		payload["cachedInputTokens"] = u.CachedInputTokens
	}
	if u.CacheWriteTokens > 0 {
		payload["cacheWriteTokens"] = u.CacheWriteTokens
	}
	if u.ReasoningTokens > 0 {
		payload["reasoningTokens"] = u.ReasoningTokens
	}
	return payload
}

// DayBucket aggregates usage events sharing the same UTC calendar date.
type DayBucket struct {
	Date         string  `json:"date"`
	Requests     int     `json:"requests"`
	InputTokens  int     `json:"inputTokens"`
	OutputTokens int     `json:"outputTokens"`
	TotalTokens  int     `json:"totalTokens"`
	TotalCost    float64 `json:"totalCost"`
}

// Totals carries report-wide sums across every bucket.
type Totals struct {
	Requests     int     `json:"requests"`
	InputTokens  int     `json:"inputTokens"`
	OutputTokens int     `json:"outputTokens"`
	TotalTokens  int     `json:"totalTokens"`
	TotalCost    float64 `json:"totalCost"`
}

// Report is the aggregated usage view rendered by `zero usage report`. Cost is a
// reconstructed estimate (see usageEventPayload) and NetLOC is a working-tree
// estimate; both are surfaced as estimates in the rendered output.
type Report struct {
	Buckets         []DayBucket `json:"buckets"`
	Total           Totals      `json:"total"`
	NetLOC          int         `json:"netLOC"`
	NetLOCPositive  bool        `json:"netLOCPositive"`
	TokensPerNetLOC float64     `json:"tokensPerNetLOC"`
	CostPerNetLOC   float64     `json:"costPerNetLOC"`
	LOCEstimated    bool        `json:"locEstimated"`
	CostEstimated   bool        `json:"costEstimated"`
}

// BuildReport aggregates persisted EventUsage events into per-day buckets and a
// report-wide total, reconstructing cost from the owning session's
// Metadata.ModelID via modelregistry.CalculateCost. Sessions whose model id is
// empty or unknown contribute token counts but no cost. The per-net-LOC ratios
// are guarded against a non-positive netLOC.
func BuildReport(events []sessions.Event, meta []sessions.Metadata, registry *modelregistry.Registry, netLOC int) (Report, error) {
	modelBySession := map[string]string{}
	for _, m := range meta {
		modelBySession[m.SessionID] = m.ModelID
	}

	buckets := map[string]*DayBucket{}
	report := Report{
		NetLOC:        netLOC,
		LOCEstimated:  true,
		CostEstimated: true,
	}

	for _, event := range events {
		if event.Type != sessions.EventUsage {
			continue
		}
		var payload usageEventPayload
		if len(event.Payload) > 0 {
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return Report{}, err
			}
		}

		date := utcDayBucket(event.CreatedAt)
		bucket, ok := buckets[date]
		if !ok {
			bucket = &DayBucket{Date: date}
			buckets[date] = bucket
		}

		bucket.Requests++
		bucket.InputTokens += payload.PromptTokens
		bucket.OutputTokens += payload.CompletionTokens
		bucket.TotalTokens += payload.TotalTokens

		report.Total.Requests++
		report.Total.InputTokens += payload.PromptTokens
		report.Total.OutputTokens += payload.CompletionTokens
		report.Total.TotalTokens += payload.TotalTokens

		// Prefer the model the event itself recorded (set on escalation runs, where
		// the model changed mid-run) so that usage is priced at the model actually
		// used; fall back to the session's model otherwise.
		modelID := payload.Model
		if modelID == "" {
			modelID = modelBySession[event.SessionID]
		}
		if modelID == "" || registry == nil {
			continue
		}
		model, err := registry.Require(modelID)
		if err != nil {
			continue
		}
		cost, err := modelregistry.CalculateCost(model, zeroruntime.Usage{
			InputTokens:       payload.PromptTokens,
			OutputTokens:      payload.CompletionTokens,
			CachedInputTokens: payload.CachedInputTokens,
			CacheWriteTokens:  payload.CacheWriteTokens,
			ReasoningTokens:   payload.ReasoningTokens,
		})
		if err != nil {
			continue
		}
		bucket.TotalCost += cost.TotalCost
		report.Total.TotalCost += cost.TotalCost
	}

	report.Buckets = make([]DayBucket, 0, len(buckets))
	for _, bucket := range buckets {
		report.Buckets = append(report.Buckets, *bucket)
	}
	sort.SliceStable(report.Buckets, func(left int, right int) bool {
		return report.Buckets[left].Date < report.Buckets[right].Date
	})

	if netLOC > 0 {
		report.NetLOCPositive = true
		report.TokensPerNetLOC = float64(report.Total.TotalTokens) / float64(netLOC)
		report.CostPerNetLOC = report.Total.TotalCost / float64(netLOC)
	}
	return report, nil
}

// utcDayBucket maps an RFC3339 timestamp to its UTC calendar date (YYYY-MM-DD).
// Normalizing to UTC first keeps an offset timestamp (e.g. ...T23:30:00-07:00,
// which is the next UTC day) bucketed by its true UTC day. On a parse failure it
// falls back to the leading-10 slice so malformed timestamps still bucket
// defensively rather than collapsing into one empty-string bucket.
func utcDayBucket(createdAt string) string {
	if parsed, err := time.Parse(time.RFC3339, createdAt); err == nil {
		return parsed.UTC().Format("2006-01-02")
	}
	if len(createdAt) >= 10 {
		return createdAt[:10]
	}
	return createdAt
}
