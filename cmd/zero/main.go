package main

import (
	"os"

	"github.com/Twigpine/zero/internal/cli"
	"github.com/Twigpine/zero/internal/sandbox"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == sandbox.LinuxSandboxHelperSubcommand {
		os.Exit(sandbox.RunLinuxSandboxHelper(os.Args[1:], os.Stderr))
	}
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
