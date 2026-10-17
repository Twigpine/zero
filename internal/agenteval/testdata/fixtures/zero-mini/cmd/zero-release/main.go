package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Twigpine/zero-fixture/internal/release"
)

func smokeTarget(alreadyBuilt bool) string {
	return release.SmokeTarget(alreadyBuilt)
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("command required: build or smoke")
	}
	switch args[0] {
	case "build":
		_, err := fmt.Fprintln(stdout, smokeTarget(false))
		return err
	case "smoke":
		_, err := fmt.Fprintln(stdout, smokeTarget(true))
		return err
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
