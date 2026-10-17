package main

import (
	"os"

	"github.com/Twigpine/zero/internal/sandbox"
)

func main() {
	os.Exit(sandbox.RunWindowsSandboxCommandRunner(os.Args[1:], os.Stderr))
}
