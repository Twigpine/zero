// Test seams: helpers only test code uses, kept out of the production binary.
package specialist

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/Twigpine/zero/internal/background"
	"github.com/Twigpine/zero/internal/streamjson"
)

func NewOutputTool(manager *background.Manager) *OutputTool {
	return &OutputTool{manager: manager}
}

func NewStopTool(manager *background.Manager) *StopTool {
	return &StopTool{manager: manager}
}

func ParseStream(reader io.Reader) ([]streamjson.Event, error) {
	// Read with a bufio.Reader rather than bufio.Scanner: a Scanner caps a single
	// token at 64 KiB/1 MiB and returns bufio.ErrTooLong past it, which aborted the
	// whole specialist run when a child emitted one large stream-json line (a big
	// tool result or final answer). ReadString has no per-line limit; the child is
	// our own trusted subprocess, so its line length is the legitimate bound.
	buffered := bufio.NewReader(reader)
	events := []streamjson.Event{}
	lineNumber := 0
	for {
		raw, readErr := buffered.ReadString('\n')
		if len(raw) > 0 {
			lineNumber++
			if line := strings.TrimSpace(raw); line != "" {
				var event streamjson.Event
				if err := json.Unmarshal([]byte(line), &event); err != nil {
					return nil, fmt.Errorf("parse stream-json line %d: %w", lineNumber, err)
				}
				if event.Type == "" {
					return nil, fmt.Errorf("parse stream-json line %d: type is required", lineNumber)
				}
				events = append(events, event)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return nil, fmt.Errorf("read stream-json output: %w", readErr)
		}
	}
	return events, nil
}
