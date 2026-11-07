package usage

import (
	"fmt"
	"time"

	"github.com/Twigpine/zero/internal/modelregistry"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

type Normalized struct {
	InputTokens       int
	CachedInputTokens int
	CacheWriteTokens  int
	OutputTokens      int
	ReasoningTokens   int
	TotalTokens       int
}

type RecordInput struct {
	ModelID string
	Usage   zeroruntime.Usage
	Source  string
}

type Record struct {
	ID        string
	Sequence  int
	ModelID   string
	Provider  modelregistry.ProviderKind
	Source    string
	CreatedAt string
	Usage     Normalized
	Cost      modelregistry.CostBreakdown
}

type ModelSummary struct {
	ModelID            string
	Provider           modelregistry.ProviderKind
	RecordCount        int
	InputTokens        int
	CachedInputTokens  int
	CacheWriteTokens   int
	OutputTokens       int
	ReasoningTokens    int
	TotalTokens        int
	InputCost          float64
	CachedInputCost    float64
	CacheWriteCost     float64
	OutputCost         float64
	TotalCost          float64
	FormattedTotalCost string
}

type Summary struct {
	RecordCount        int
	Currency           string
	InputTokens        int
	CachedInputTokens  int
	CacheWriteTokens   int
	OutputTokens       int
	ReasoningTokens    int
	TotalTokens        int
	InputCost          float64
	CachedInputCost    float64
	CacheWriteCost     float64
	OutputCost         float64
	TotalCost          float64
	FormattedTotalCost string
	ByModel            []ModelSummary
	LastRecord         *Record
}

type TrackerOptions struct {
	Now      func() time.Time
	Registry *modelregistry.Registry
}

type Tracker struct {
	now      func() time.Time
	registry modelregistry.Registry
	records  []Record
	nextSeq  int
}

func NewTracker(options TrackerOptions) *Tracker {
	registry := options.Registry
	if registry == nil {
		defaultRegistry, err := modelregistry.DefaultRegistry()
		if err != nil {
			panic(err)
		}
		registry = &defaultRegistry
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Tracker{now: now, registry: *registry, nextSeq: 1}
}

func (tracker *Tracker) Record(input RecordInput) (Record, error) {
	model, err := tracker.registry.Require(input.ModelID)
	if err != nil {
		return Record{}, err
	}
	normalized, runtimeUsage, err := Normalize(input.Usage)
	if err != nil {
		return Record{}, err
	}
	cost, err := modelregistry.CalculateCost(model, runtimeUsage)
	if err != nil {
		return Record{}, err
	}
	sequence := tracker.nextSeq
	tracker.nextSeq++
	record := Record{
		ID:        fmt.Sprintf("zero_usage_%d", sequence),
		Sequence:  sequence,
		ModelID:   model.ID,
		Provider:  model.Provider,
		Source:    input.Source,
		CreatedAt: tracker.now().UTC().Format(time.RFC3339),
		Usage:     normalized,
		Cost:      cost,
	}
	tracker.records = append(tracker.records, record)
	return record, nil
}

func (tracker *Tracker) Records() []Record {
	return append([]Record{}, tracker.records...)
}

func (tracker *Tracker) Summary() Summary {
	summary := Summary{Currency: "USD"}
	models := map[string]int{}
	for _, record := range tracker.records {
		summary.RecordCount++
		addUsageToSummary(&summary, record.Usage)
		addCostToSummary(&summary, record.Cost)
		index, ok := models[record.ModelID]
		if !ok {
			summary.ByModel = append(summary.ByModel, ModelSummary{
				ModelID:  record.ModelID,
				Provider: record.Provider,
			})
			index = len(summary.ByModel) - 1
			models[record.ModelID] = index
		}
		addUsageToModel(&summary.ByModel[index], record.Usage)
		addCostToModel(&summary.ByModel[index], record.Cost)
		recordCopy := record
		summary.LastRecord = &recordCopy
	}
	summary.FormattedTotalCost = formatCost(summary.TotalCost)
	for index := range summary.ByModel {
		summary.ByModel[index].FormattedTotalCost = formatCost(summary.ByModel[index].TotalCost)
	}
	return summary
}

func (tracker *Tracker) Reset() {
	tracker.records = nil
	tracker.nextSeq = 1
}

func Normalize(usage zeroruntime.Usage) (Normalized, zeroruntime.Usage, error) {
	inputTokens, err := nonNegative(firstNonZero(usage.InputTokens, usage.PromptTokens), "inputTokens")
	if err != nil {
		return Normalized{}, zeroruntime.Usage{}, err
	}
	outputTokens, err := nonNegative(firstNonZero(usage.OutputTokens, usage.CompletionTokens), "outputTokens")
	if err != nil {
		return Normalized{}, zeroruntime.Usage{}, err
	}
	cachedInputTokens, err := nonNegative(usage.CachedInputTokens, "cachedInputTokens")
	if err != nil {
		return Normalized{}, zeroruntime.Usage{}, err
	}
	if cachedInputTokens > inputTokens {
		cachedInputTokens = inputTokens
	}
	cacheWriteTokens, err := nonNegative(usage.CacheWriteTokens, "cacheWriteTokens")
	if err != nil {
		return Normalized{}, zeroruntime.Usage{}, err
	}
	if cacheWriteTokens > inputTokens-cachedInputTokens {
		cacheWriteTokens = inputTokens - cachedInputTokens
	}
	reasoningTokens, err := nonNegative(usage.ReasoningTokens, "reasoningTokens")
	if err != nil {
		return Normalized{}, zeroruntime.Usage{}, err
	}
	normalized := Normalized{
		InputTokens:       inputTokens,
		CachedInputTokens: cachedInputTokens,
		CacheWriteTokens:  cacheWriteTokens,
		OutputTokens:      outputTokens,
		ReasoningTokens:   reasoningTokens,
		TotalTokens:       inputTokens + outputTokens,
	}
	return normalized, zeroruntime.Usage{
		InputTokens:       inputTokens,
		PromptTokens:      inputTokens,
		CachedInputTokens: cachedInputTokens,
		CacheWriteTokens:  cacheWriteTokens,
		OutputTokens:      outputTokens,
		CompletionTokens:  outputTokens,
		ReasoningTokens:   reasoningTokens,
	}, nil
}

func FormatSummary(summary Summary) string {
	requestLabel := "requests"
	if summary.RecordCount == 1 {
		requestLabel = "request"
	}
	return fmt.Sprintf("%s %s, %s tokens, %s", comma(summary.RecordCount), requestLabel, comma(summary.TotalTokens), summary.FormattedTotalCost)
}

// CacheHitRate is the fraction of input tokens served from the provider's prompt
// cache. CachedInputTokens is clamped to InputTokens in Normalize, so the result is
// always in [0,1]; it is 0 when no input has been recorded.
func (summary Summary) CacheHitRate() float64 {
	if summary.InputTokens <= 0 {
		return 0
	}
	return float64(summary.CachedInputTokens) / float64(summary.InputTokens)
}

// FormatCacheEfficiency renders the prompt-cache hit rate for display, e.g.
// "63% (8,200 cached / 13,100 input)", so a user can see whether cache reads are
// actually saving work. Returns "n/a" until some input has been recorded.
func FormatCacheEfficiency(summary Summary) string {
	if summary.InputTokens <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%% (%s cached / %s input)",
		summary.CacheHitRate()*100,
		comma(summary.CachedInputTokens),
		comma(summary.InputTokens))
}

func addUsageToSummary(summary *Summary, usage Normalized) {
	summary.InputTokens += usage.InputTokens
	summary.CachedInputTokens += usage.CachedInputTokens
	summary.CacheWriteTokens += usage.CacheWriteTokens
	summary.OutputTokens += usage.OutputTokens
	summary.ReasoningTokens += usage.ReasoningTokens
	summary.TotalTokens += usage.TotalTokens
}

func addUsageToModel(summary *ModelSummary, usage Normalized) {
	summary.RecordCount++
	summary.InputTokens += usage.InputTokens
	summary.CachedInputTokens += usage.CachedInputTokens
	summary.CacheWriteTokens += usage.CacheWriteTokens
	summary.OutputTokens += usage.OutputTokens
	summary.ReasoningTokens += usage.ReasoningTokens
	summary.TotalTokens += usage.TotalTokens
}

func addCostToSummary(summary *Summary, cost modelregistry.CostBreakdown) {
	summary.InputCost += cost.InputCost
	summary.CachedInputCost += cost.CachedInputCost
	summary.CacheWriteCost += cost.CacheWriteCost
	summary.OutputCost += cost.OutputCost
	summary.TotalCost += cost.TotalCost
}

func addCostToModel(summary *ModelSummary, cost modelregistry.CostBreakdown) {
	summary.InputCost += cost.InputCost
	summary.CachedInputCost += cost.CachedInputCost
	summary.CacheWriteCost += cost.CacheWriteCost
	summary.OutputCost += cost.OutputCost
	summary.TotalCost += cost.TotalCost
}

func formatCost(value float64) string {
	formatted, err := modelregistry.FormatCostUSD(value)
	if err != nil {
		return "$0.0000"
	}
	return formatted
}

func nonNegative(value int, label string) (int, error) {
	if value < 0 {
		return 0, fmt.Errorf("expected %s to be non-negative", label)
	}
	return value, nil
}

func firstNonZero(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func comma(value int) string {
	sign := ""
	if value < 0 {
		sign = "-"
		value = -value
	}
	digits := fmt.Sprintf("%d", value)
	if len(digits) <= 3 {
		return sign + digits
	}
	var out []byte
	prefix := len(digits) % 3
	if prefix == 0 {
		prefix = 3
	}
	out = append(out, digits[:prefix]...)
	for index := prefix; index < len(digits); index += 3 {
		out = append(out, ',')
		out = append(out, digits[index:index+3]...)
	}
	return sign + string(out)
}
