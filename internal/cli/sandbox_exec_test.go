package cli

import (
	"errors"
	"strings"
	"testing"
)

// parseSandboxExecArgs sits on the boundary between Zero's flags and a child
// command's, and it had no tests at all: not for the reported defect, not even
// for the happy case. The contract it has to keep is that everything after the
// separator belongs to the child, help flags included, so the wrapper can never
// answer for a command it was supposed to be running.
func TestParseSandboxExecArgs(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		args    []string
		command []string
		help    bool
		usage   bool
	}{
		{
			// The reported defect: --help after the separator is the CHILD's.
			name:    "help after the separator belongs to the child",
			args:    []string{"--", "cmd", "--help"},
			command: []string{"cmd", "--help"},
		},
		{
			name:    "a second separator is the child's too",
			args:    []string{"--", "cmd", "--", "inner"},
			command: []string{"cmd", "--", "inner"},
		},
		{
			name:    "a flag-looking command survives the separator",
			args:    []string{"--", "--weird-binary"},
			command: []string{"--weird-binary"},
		},
		{
			// The separator-less form. The first token is the command, so its own
			// flags are not ours to read either.
			name:    "help after a separatorless command belongs to the child",
			args:    []string{"cmd", "--help"},
			command: []string{"cmd", "--help"},
		},
		{
			// A separator arriving AFTER the command has already started is part of
			// the child's argv, not a delimiter we get a second go at.
			name:    "a separator after the command is the child's argument",
			args:    []string{"ls", "--", "foo"},
			command: []string{"ls", "--", "foo"},
		},
		{name: "bare help", args: []string{"--help"}, help: true},
		{name: "short help", args: []string{"-h"}, help: true},
		{name: "help subcommand", args: []string{"help"}, help: true},
		{name: "help in the prefix wins", args: []string{"--help", "--", "cmd"}, help: true},
		{name: "no arguments", args: nil, usage: true},
		{name: "separator with nothing after it", args: []string{"--"}, usage: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			command, err := parseSandboxExecArgs(testCase.args)
			switch {
			case testCase.help:
				if !errors.Is(err, errSandboxExecHelp) {
					t.Fatalf("parseSandboxExecArgs(%q) = %q, %v; want the help sentinel", testCase.args, command, err)
				}
			case testCase.usage:
				if err == nil || errors.Is(err, errSandboxExecHelp) {
					t.Fatalf("parseSandboxExecArgs(%q) = %q, %v; want a usage error", testCase.args, command, err)
				}
				if !strings.Contains(err.Error(), "usage:") {
					t.Errorf("a usage error must show the usage, got %q", err.Error())
				}
			default:
				if err != nil {
					t.Fatalf("parseSandboxExecArgs(%q) returned %v", testCase.args, err)
				}
				if strings.Join(command, "\x00") != strings.Join(testCase.command, "\x00") {
					t.Errorf("parseSandboxExecArgs(%q) = %q, want %q", testCase.args, command, testCase.command)
				}
			}
		})
	}
}

// The wrapper must never consume a token it then fails to hand over. Whatever it
// returns as the command has to be a trailing slice of what it was given, with
// at most a leading separator removed, or an argument has gone missing on its
// way to the child.
func TestParseSandboxExecArgsNeverDropsAChildArgument(t *testing.T) {
	for _, args := range [][]string{
		{"--", "cmd", "--help", "-x", "--"},
		{"cmd", "--help"},
		{"ls", "--", "foo"},
		{"--", "--weird-binary", "arg"},
	} {
		command, err := parseSandboxExecArgs(args)
		if err != nil {
			t.Fatalf("parseSandboxExecArgs(%q) returned %v", args, err)
		}
		if len(command) == 0 {
			t.Fatalf("parseSandboxExecArgs(%q) returned an empty command", args)
		}
		suffix := strings.Join(args[len(args)-len(command):], "\x00")
		if strings.Join(command, "\x00") != suffix {
			t.Errorf("parseSandboxExecArgs(%q) = %q, which is not a trailing slice of the input; an argument was dropped or reordered", args, command)
		}
	}
}
