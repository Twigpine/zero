package main

import (
	"os"

	"github.com/Twigpine/zero/internal/sandbox"
)

func main() {
	os.Exit(sandbox.RunWindowsSandboxSetup(os.Args[1:], os.Stderr))
}
