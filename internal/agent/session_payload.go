package agent

import (
	"strings"

	"github.com/Twigpine/zero/internal/tools"
)

// CardBody is the card body source: the rich card-only Display.Preview (a
// code/diff preview) when present on a successful result, else the text the
// model also saw. Error results keep their output so the failure shows.
//
// UNDECORATED, ALWAYS. The enforcement disclosure is carried separately as typed
// notices and rendered by the card as its own furniture, so a body that already
// had the notice composed into it drew the warning twice: once in the notice
// lines and once at the top of the output. A rich preview never carried it, so
// only the no-preview results (every bash and exec card, and every error) were
// wrong, which is exactly the shape a preview-only test cannot see.
//
// One owner for composition: this returns the base text, and whoever presents it
// decorates once. Provider-facing text still goes through ModelOutput.
func (result ToolResult) CardBody() string {
	display := result.BaseDisplay()
	if strings.TrimSpace(display.Preview) != "" && (result.Status != tools.StatusError || result.Outcome.Finalized()) {
		return display.Preview
	}
	return result.BaseModelOutput()
}

// ToolResultSessionPayload is THE serialization of a tool result into a session
// event, shared by every writer: the TUI, headless exec, and the ACP agent.
//
// It lives here, beside ToolResult, because it has three callers and only one of
// them is a terminal. The writers used to spell the payload separately, and each
// time one of them was left behind the same thing went missing on reload. The
// headless writer persisted only the decorated ModelOutput, so a CLI-written
// result restored into the TUI arrived with no typed notices and no undecorated
// body. The ACP writer persisted the raw Output field, which after the
// output/notice split is the UNDECORATED text, so an ACP client that loaded a
// session saw every sandboxed result with its disclosure gone, although the
// live update for the same call had shown it. All of them write to the same
// session store, so a session written by one surface is restored by another.
// One owner for the contract means one place where a field can go missing, and a
// test against this function covers every writer.
//
// output is the provider-facing text with the disclosure composed in.
// displayPreview is the undecorated card body, written whenever it differs from
// output. A result carrying enforcement notices ALWAYS differs, because output
// is decorated and the card body is not, so the undecorated body is written
// even when it is empty. Readers key on the field being PRESENT rather than
// non-empty for exactly that case: a command that printed nothing under an
// enforced profile has an empty body and a real notice, and falling back to
// output there would restore the decorated text and draw the notice twice.
func ToolResultSessionPayload(result ToolResult) map[string]any {
	output := result.ModelOutput()
	payload := map[string]any{
		"toolCallId": result.ToolCallID,
		"name":       result.Name,
		"status":     string(result.Status),
		"output":     output,
	}
	if preview := result.CardBody(); preview != output {
		payload["displayPreview"] = preview
	}
	if result.Truncated {
		payload["truncated"] = true
	}
	if result.Redacted {
		payload["redacted"] = true
	}
	if len(result.Meta) > 0 {
		payload["meta"] = result.Meta
	}
	if len(result.EnforcementNotices) > 0 {
		payload["enforcementNotices"] = result.EnforcementNotices
	}
	if len(result.ChangedFiles) > 0 {
		payload["changedFiles"] = result.ChangedFiles
	}
	if len(result.ChangeSummaries) > 0 {
		payload["changeSummaries"] = result.ChangeSummaries
	}
	return payload
}
