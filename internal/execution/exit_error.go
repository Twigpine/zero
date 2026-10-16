package execution

import "os/exec"

// AsPureExitError reports whether err is an ordinary process exit or a join
// tree containing only ordinary process exits. It does not unwrap single-error
// wrappers, which may carry a distinct lifecycle or cleanup failure.
func AsPureExitError(err error) (*exec.ExitError, bool) {
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr, exitErr != nil
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return nil, false
	}
	causes := joined.Unwrap()
	if len(causes) == 0 {
		return nil, false
	}
	var first *exec.ExitError
	for _, cause := range causes {
		exitErr, ok := AsPureExitError(cause)
		if !ok {
			return nil, false
		}
		if first == nil {
			first = exitErr
		}
	}
	return first, first != nil
}
