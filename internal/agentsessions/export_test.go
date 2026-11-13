// Test seams: helpers only test code uses, kept out of the production binary.
package agentsessions

import "github.com/Twigpine/zero/internal/sessions"

// The production readers take an open handle (see openSelectedSource) so the
// identity that was verified is the one that is read. Tests that only care
// about translation still build a file and name it; these wrappers open it the
// same way production does and hand the handle through.
func translateFamily1At(root string, path string, options ReadOptions) ([]sessions.AppendEventInput, error) {
	file, err := openContained(root, path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return translateFamily1(file, options)
}

func translateCodexAt(root string, path string, options ReadOptions) ([]sessions.AppendEventInput, error) {
	return translateCodexAtWithByteLimit(root, path, options, importByteLimit)
}

func translateCodexAtWithByteLimit(root string, path string, options ReadOptions, maxBytes int) ([]sessions.AppendEventInput, error) {
	file, err := openContained(root, path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return translateCodexWithByteLimit(file, options, maxBytes)
}

func streamTailLinesAt(root string, path string, maxLineBytes int, maxBytes int, visit func(line []byte, truncated bool) bool) (bool, error) {
	file, err := openContained(root, path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	return streamTailLines(file, maxLineBytes, maxBytes, visit)
}
