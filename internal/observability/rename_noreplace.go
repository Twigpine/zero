package observability

import (
	"os"

	"github.com/Twigpine/zero/internal/pathjail"
)

// renameNoReplace publishes a crash report without replacing an existing one.
// The implementation lives in pathjail because the file tools need the same
// atomic no-replace guarantee.
func renameNoReplace(root *os.Root, oldname, newname string) error {
	return pathjail.RenameNoReplace(root, oldname, newname)
}
