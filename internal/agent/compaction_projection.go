package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/Gitlawb/zero/internal/zeroruntime"
)

const (
	compactionUserWordBudget       = 256
	compactionAssistantWordBudget  = 200
	compactionToolCallsPerTurn     = 8
	compactionToolArgumentBytes    = 120
	compactionErrorBytes           = 150
	compactionResultBytes          = 1200
	compactionPreviousSummaryBytes = 16 * 1024
	compactionBriefMaxBytes        = 24 * 1024
)

var compactionWordPattern = regexp.MustCompile(`[\p{L}\p{N}]+`)

type compactionProjection struct {
	messages  []zeroruntime.Message
	truncated bool
}

// projectCompactionInput removes reconstructible tool output before the model
// summary call. It retains a bounded transcript of user intent, assistant
// decisions, tool actions, and concise errors. Exact durable state is appended
// separately from the original messages after the summary returns.
func projectCompactionInput(messages []zeroruntime.Message) compactionProjection {
	sections := make([]string, 0, len(messages))
	previousSummary := ""
	truncated := false
	for index, message := range messages {
		switch message.Role {
		case zeroruntime.MessageRoleUser:
			content := message.Content
			if marker := strings.Index(content, preservedStateLabel); marker >= 0 {
				content = content[:marker]
			}
			trimmed := strings.TrimSpace(content)
			if strings.HasPrefix(trimmed, summaryLabel) {
				fullSummary := strings.TrimSpace(strings.TrimPrefix(trimmed, summaryLabel))
				previousSummary = clipHeadTailBytes(fullSummary, compactionPreviousSummaryBytes)
				truncated = truncated || len(fullSummary) > compactionPreviousSummaryBytes
				continue
			}
			if content = clipInformativeWords(content, compactionUserWordBudget); content != "" {
				sections = append(sections, fmt.Sprintf("[user #%d]\n%s", index, content))
			}
		case zeroruntime.MessageRoleAssistant:
			lines := make([]string, 0, 1+len(message.ToolCalls))
			if content := clipInformativeWords(message.Content, compactionAssistantWordBudget); content != "" {
				lines = append(lines, content)
			}
			calls := message.ToolCalls
			omitted := 0
			if len(calls) > compactionToolCallsPerTurn {
				omitted = len(calls) - compactionToolCallsPerTurn
				calls = calls[omitted:]
			}
			if omitted > 0 {
				lines = append(lines, fmt.Sprintf("* (%d earlier tool calls omitted)", omitted))
			}
			for _, call := range calls {
				lines = append(lines, compactionToolCallLine(call))
			}
			if len(lines) > 0 {
				sections = append(sections, fmt.Sprintf("[assistant #%d]\n%s", index, strings.Join(lines, "\n")))
			}
		case zeroruntime.MessageRoleTool:
			// New tool-result messages carry the execution status directly, as the
			// source ToolResult does. Keep the text check for older session history
			// created before Message exposed IsError.
			name := toolNameForResult(messages, index)
			mustPreserve := name == "ask_user" || len(message.ChangedFiles) > 0
			if !mustPreserve && !message.IsError && !isLikelyToolError(message.Content) {
				continue
			}
			if name == "" {
				name = "tool"
			}
			switch {
			case message.IsError || isLikelyToolError(message.Content):
				sections = append(sections, fmt.Sprintf("[tool_error #%d] %s\n%s", index, name, clipBytes(firstLine(message.Content), compactionErrorBytes)))
			case name == "ask_user":
				sections = append(sections, fmt.Sprintf("[user_answer #%d] %s\n%s", index, name, clipBytes(message.Content, compactionResultBytes)))
			default:
				sections = append(sections, fmt.Sprintf("[tool_result #%d] %s changed %s\n%s", index, name,
					clipBytes(strings.Join(message.ChangedFiles, ", "), compactionToolArgumentBytes), clipBytes(firstLine(message.Content), compactionErrorBytes)))
			}
		}
	}
	if len(sections) == 0 && previousSummary == "" {
		return compactionProjection{}
	}
	brief, briefTruncated := capCompactionBrief(strings.Join(sections, "\n\n"))
	truncated = truncated || briefTruncated
	if previousSummary != "" {
		brief = "[previous summary]\n" + previousSummary + "\n\n" + brief
	}
	return compactionProjection{
		messages:  []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: strings.TrimSpace(brief)}},
		truncated: truncated,
	}
}

func capCompactionBrief(brief string) (string, bool) {
	if len(brief) <= compactionBriefMaxBytes {
		return brief, false
	}
	marker := "\n\n...[middle transcript omitted to fit compaction budget]...\n\n"
	available := compactionBriefMaxBytes - len(marker)
	// Keep both chronological edges: the oldest material has not necessarily
	// been summarized before, while the newest material is most actionable.
	headBytes := available * 2 / 5
	tailBytes := available - headBytes
	head := clipPrefixAtBoundary(brief, headBytes)
	tail := clipSuffixAtBoundary(brief, tailBytes)
	return strings.TrimSpace(head) + marker + strings.TrimSpace(tail), true
}

func compactionToolCallLine(call zeroruntime.ToolCall) string {
	detail := ""
	if call.Freeform && call.Name == "apply_patch" {
		detail = strings.Join(patchPaths(call.Arguments), ", ")
	}
	var arguments map[string]any
	if detail == "" && json.Unmarshal([]byte(strings.TrimSpace(call.Arguments)), &arguments) == nil {
		for _, key := range []string{"file_path", "path", "url", "query", "pattern", "prompt", "description", "question"} {
			if value, ok := arguments[key].(string); ok && strings.TrimSpace(value) != "" {
				detail = value
				break
			}
		}
		if detail == "" && call.Name == "ask_user" {
			detail = askUserQuestionsDetail(arguments)
		}
		if detail == "" && call.Name == "apply_patch" {
			if patch, ok := arguments["patch"].(string); ok {
				detail = strings.Join(patchPaths(patch), ", ")
			}
		}
	}
	if detail == "" {
		detail = commandFromArguments(call.Arguments)
	}
	if detail == "" {
		return "* " + call.Name
	}
	return fmt.Sprintf("* %s %q", call.Name, clipBytes(detail, compactionToolArgumentBytes))
}

func isLikelyToolError(content string) bool {
	prefix := strings.TrimSpace(content)
	if len(prefix) > compactionErrorBytes {
		prefix = prefix[:compactionErrorBytes]
	}
	lower := strings.ToLower(prefix)
	for _, prefix := range []string{"error:", "tool error:", "failed:", "permission denied", "command failed"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func clipInformativeWords(text string, limit int) string {
	flat := strings.Join(strings.Fields(text), " ")
	if flat == "" || limit <= 0 {
		return ""
	}
	count := 0
	for _, location := range compactionWordPattern.FindAllStringIndex(flat, -1) {
		word := strings.ToLower(flat[location[0]:location[1]])
		if compactionStopWords[word] {
			continue
		}
		count++
		if count > limit {
			end := location[0]
			return strings.TrimSpace(flat[:end]) + "...(truncated)"
		}
	}
	return flat
}

func clipBytes(text string, limit int) string {
	text = strings.TrimSpace(text)
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	if limit <= 3 {
		end := limit
		for end > 0 && end < len(text) && text[end]&0xc0 == 0x80 {
			end--
		}
		return text[:end]
	}
	end := limit - 3
	for end > 0 && end < len(text) && text[end]&0xc0 == 0x80 {
		end--
	}
	return strings.TrimSpace(text[:end]) + "..."
}

func clipHeadTailBytes(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	if limit <= 0 {
		return ""
	}
	marker := "\n...[middle omitted]...\n"
	if limit <= len(marker) {
		return clipBytes(text, limit)
	}
	available := limit - len(marker)
	head := clipPrefixAtBoundary(text, available/2)
	tail := clipSuffixAtBoundary(text, available-available/2)
	return strings.TrimSpace(head) + marker + strings.TrimSpace(tail)
}

func clipPrefixAtBoundary(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	end := limit
	for end > 0 && text[end]&0xc0 == 0x80 {
		end--
	}
	if boundary := strings.LastIndex(text[:end], "\n\n["); boundary > 0 {
		end = boundary
	}
	return text[:end]
}

func clipSuffixAtBoundary(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	start := len(text) - limit
	for start < len(text) && text[start]&0xc0 == 0x80 {
		start++
	}
	if boundary := strings.Index(text[start:], "\n\n["); boundary >= 0 {
		start += boundary + 2
	}
	return text[start:]
}

func askUserQuestionsDetail(arguments map[string]any) string {
	raw, ok := arguments["questions"].([]any)
	if !ok {
		return ""
	}
	questions := make([]string, 0, len(raw))
	for _, item := range raw {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if question, ok := object["question"].(string); ok && strings.TrimSpace(question) != "" {
			questions = append(questions, strings.TrimSpace(question))
		}
	}
	return strings.Join(questions, " | ")
}

func patchPaths(patch string) []string {
	var paths []string
	seen := map[string]bool{}
	for _, line := range strings.Split(patch, "\n") {
		for _, prefix := range []string{"*** Add File: ", "*** Update File: ", "*** Delete File: ", "*** Move to: "} {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			path := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			if path != "" && !seen[path] {
				seen[path] = true
				paths = append(paths, path)
			}
		}
	}
	return paths
}

var compactionStopWords = map[string]bool{
	"a": true, "an": true, "the": true, "is": true, "are": true, "was": true, "were": true,
	"be": true, "been": true, "being": true, "have": true, "has": true, "had": true,
	"do": true, "does": true, "did": true, "will": true, "would": true, "could": true,
	"should": true, "may": true, "might": true, "shall": true, "can": true, "need": true, "must": true,
	"to": true, "of": true, "in": true, "for": true, "on": true, "with": true, "at": true,
	"by": true, "from": true, "as": true, "into": true, "through": true, "during": true,
	"before": true, "after": true, "above": true, "below": true, "between": true, "under": true, "over": true,
	"and": true, "but": true, "or": true, "nor": true, "not": true, "so": true, "yet": true,
	"both": true, "either": true, "neither": true, "each": true, "every": true, "all": true,
	"any": true, "few": true, "more": true, "most": true, "other": true, "some": true, "such": true, "no": true,
	"that": true, "this": true, "these": true, "those": true, "it": true, "its": true,
	"i": true, "me": true, "my": true, "we": true, "our": true, "you": true, "your": true,
	"he": true, "him": true, "his": true, "she": true, "her": true, "they": true, "them": true, "their": true,
	"who": true, "which": true, "what": true, "if": true, "then": true, "than": true,
	"when": true, "where": true, "how": true, "just": true, "also": true,
}
