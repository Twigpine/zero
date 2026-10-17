package cli

import (
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"strings"

	"github.com/Twigpine/zero/internal/specialist"
)

type specialistOptions struct {
	json            bool
	location        specialist.Location
	description     string
	prompt          string
	extends         string
	model           string
	reasoningEffort string
	tools           []string
	force           bool
}

func runSpecialists(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	command, remaining, options, help, err := parseSpecialistArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeSpecialistHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	if err := validateSpecialistCommand(command, remaining); err != nil {
		return writeExecUsageError(stderr, err.Error())
	}

	workspaceRoot, err := resolveWorkspaceRoot("", deps)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	paths, err := specialist.DefaultPaths(workspaceRoot)
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}

	switch command {
	case "list":
		return runSpecialistList(paths, options, stdout, stderr)
	case "show":
		return runSpecialistShow(paths, remaining[0], options, stdout, stderr)
	case "create":
		return runSpecialistCreate(paths, remaining[0], options, stdout, stderr)
	case "delete", "rm":
		return runSpecialistDelete(paths, remaining[0], options, stdout, stderr)
	case "edit":
		return runSpecialistEdit(paths, remaining[0], options, stdout, stderr, deps)
	case "path":
		return runSpecialistPath(paths, options, stdout)
	default:
		return writeExecUsageError(stderr, fmt.Sprintf("unknown specialist command %q", command))
	}
}

func parseSpecialistArgs(args []string) (string, []string, specialistOptions, bool, error) {
	command := "list"
	commandExplicit := false
	remaining := []string{}
	options := specialistOptions{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch arg {
		case "-h", "--help", "help":
			return command, remaining, options, true, nil
		case "--json":
			options.json = true
		case "--user":
			options.location = specialist.LocationUser
		case "--project":
			options.location = specialist.LocationProject
		case "--force":
			options.force = true
		case "--location", "--description", "--prompt", "--system-prompt", "--tools", "--extends", "--model", "--reasoning-effort":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return command, remaining, options, false, err
			}
			index = next
			if err := applySpecialistOption(&options, arg, value); err != nil {
				return command, remaining, options, false, err
			}
		default:
			if strings.HasPrefix(arg, "--location=") {
				value, err := requiredInlineFlagValue(arg, "--location")
				if err != nil {
					return command, remaining, options, false, err
				}
				if err := applySpecialistOption(&options, "--location", value); err != nil {
					return command, remaining, options, false, err
				}
				continue
			}
			if strings.HasPrefix(arg, "--description=") {
				value, err := requiredInlineFlagValue(arg, "--description")
				if err != nil {
					return command, remaining, options, false, err
				}
				if err := applySpecialistOption(&options, "--description", value); err != nil {
					return command, remaining, options, false, err
				}
				continue
			}
			if strings.HasPrefix(arg, "--prompt=") || strings.HasPrefix(arg, "--system-prompt=") {
				flag := "--prompt"
				if strings.HasPrefix(arg, "--system-prompt=") {
					flag = "--system-prompt"
				}
				value, err := requiredInlineFlagValue(arg, flag)
				if err != nil {
					return command, remaining, options, false, err
				}
				if err := applySpecialistOption(&options, flag, value); err != nil {
					return command, remaining, options, false, err
				}
				continue
			}
			if strings.HasPrefix(arg, "--tools=") {
				value, err := requiredInlineFlagValue(arg, "--tools")
				if err != nil {
					return command, remaining, options, false, err
				}
				if err := applySpecialistOption(&options, "--tools", value); err != nil {
					return command, remaining, options, false, err
				}
				continue
			}
			if strings.HasPrefix(arg, "--extends=") || strings.HasPrefix(arg, "--model=") || strings.HasPrefix(arg, "--reasoning-effort=") {
				flag, _, _ := strings.Cut(arg, "=")
				value, err := requiredInlineFlagValue(arg, flag)
				if err != nil {
					return command, remaining, options, false, err
				}
				if err := applySpecialistOption(&options, flag, value); err != nil {
					return command, remaining, options, false, err
				}
				continue
			}
			if strings.HasPrefix(arg, "-") {
				return command, remaining, options, false, fmt.Errorf("unknown specialist flag %q", arg)
			}
			if !commandExplicit {
				command = arg
				commandExplicit = true
			} else {
				remaining = append(remaining, arg)
			}
		}
	}
	return command, remaining, options, false, nil
}

func applySpecialistOption(options *specialistOptions, flag string, value string) error {
	value = strings.TrimSpace(value)
	switch flag {
	case "--location":
		switch strings.ToLower(value) {
		case "user":
			options.location = specialist.LocationUser
		case "project":
			options.location = specialist.LocationProject
		default:
			return fmt.Errorf("--location must be user or project")
		}
	case "--description":
		options.description = value
	case "--prompt", "--system-prompt":
		options.prompt = value
	case "--tools":
		options.tools = specialist.SplitToolList(value)
	case "--extends":
		options.extends = value
	case "--model":
		options.model = value
	case "--reasoning-effort":
		options.reasoningEffort = value
	default:
		return fmt.Errorf("unknown specialist flag %q", flag)
	}
	return nil
}

func validateSpecialistCommand(command string, remaining []string) error {
	switch command {
	case "list":
		if len(remaining) != 0 {
			return fmt.Errorf("specialist list does not accept positional arguments")
		}
	case "show":
		if len(remaining) != 1 {
			return fmt.Errorf("specialist show requires a specialist name")
		}
	case "create":
		if len(remaining) != 1 {
			return fmt.Errorf("specialist create requires a specialist name")
		}
	case "delete", "rm":
		if len(remaining) != 1 {
			return fmt.Errorf("specialist delete requires a specialist name")
		}
	case "edit":
		if len(remaining) != 1 {
			return fmt.Errorf("specialist edit requires a specialist name")
		}
	case "path":
		if len(remaining) != 0 {
			return fmt.Errorf("specialist path does not accept positional arguments")
		}
	default:
		return fmt.Errorf("unknown specialist command %q", command)
	}
	return nil
}

func runSpecialistList(paths specialist.Paths, options specialistOptions, stdout io.Writer, stderr io.Writer) int {
	result, err := specialist.Load(specialist.LoadOptions{Paths: paths})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	if options.json {
		if err := writePrettyJSON(stdout, struct {
			Paths       specialist.Paths     `json:"paths"`
			Specialists []specialist.Summary `json:"specialists"`
			Warnings    []string             `json:"warnings,omitempty"`
		}{
			Paths:       result.Paths,
			Specialists: specialist.Summaries(result.Specialists),
			Warnings:    result.Warnings,
		}); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, specialist.FormatList(result)); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runSpecialistShow(paths specialist.Paths, name string, options specialistOptions, stdout io.Writer, stderr io.Writer) int {
	result, err := specialist.Load(specialist.LoadOptions{Paths: paths})
	if err != nil {
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	manifest, ok := specialist.Find(result, name)
	if !ok {
		return writeExecUsageError(stderr, "Zero specialist not found: "+name)
	}
	if options.json {
		if err := writePrettyJSON(stdout, manifest); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, specialist.FormatShow(manifest)); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runSpecialistCreate(paths specialist.Paths, name string, options specialistOptions, stdout io.Writer, stderr io.Writer) int {
	storage := specialist.NewStorage(paths)
	manifest, err := storage.Create(specialist.CreateInput{
		Name:            name,
		Description:     options.description,
		SystemPrompt:    options.prompt,
		Extends:         options.extends,
		Model:           options.model,
		ReasoningEffort: options.reasoningEffort,
		Tools:           options.tools,
		Location:        options.location,
		Overwrite:       options.force,
	})
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	return writeSpecialistCreateResult(manifest, options.json, stdout, stderr)
}

func writeSpecialistCreateResult(manifest specialist.Manifest, jsonOutput bool, stdout io.Writer, stderr io.Writer) int {
	if jsonOutput {
		if err := writePrettyJSON(stdout, manifest); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintf(stdout, "Created specialist %s at %s\n", manifest.Metadata.Name, manifest.FilePath); err != nil {
		return exitCrash
	}
	for _, warning := range manifest.Warnings {
		if _, err := fmt.Fprintf(stderr, "warning: %s\n", warning); err != nil {
			return exitCrash
		}
	}
	return exitSuccess
}

func runSpecialistDelete(paths specialist.Paths, name string, options specialistOptions, stdout io.Writer, stderr io.Writer) int {
	storage := specialist.NewStorage(paths)
	path, err := storage.Delete(specialist.DeleteInput{Name: name, Location: options.location})
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if options.json {
		if err := writePrettyJSON(stdout, map[string]any{"name": name, "path": path, "deleted": true}); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintf(stdout, "Deleted specialist %s at %s\n", name, path); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runSpecialistEdit(paths specialist.Paths, name string, options specialistOptions, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	storage := specialist.NewStorage(paths)
	path, err := storage.Path(name, options.location)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			for _, b := range specialist.Builtins() {
				if b.Metadata.Name == name {
					return writeExecUsageError(stderr, fmt.Sprintf("cannot edit builtin specialist %q: builtins are read-only. Create a user or project specialist of the same name to override it.", name))
				}
			}
			return writeExecUsageError(stderr, "specialist not found: "+name)
		}
		return writeAppError(stderr, err.Error(), exitCrash)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return writeExecUsageError(stderr, "refusing to edit symlink specialist file: "+path)
	}
	runEditor := deps.runEditor
	if runEditor == nil {
		runEditor = openEditor
	}
	if err := runEditor(path); err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if _, err := fmt.Fprintf(stdout, "Edited specialist %s at %s\n", name, path); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runSpecialistPath(paths specialist.Paths, options specialistOptions, stdout io.Writer) int {
	if options.json {
		if err := writePrettyJSON(stdout, paths); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, specialist.FormatPaths(paths)); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func writeSpecialistHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero specialist [command] [flags]

Commands:
  list       List built-in, user, and project specialists
  show NAME  Show one specialist profile
  create NAME
             Create a user specialist profile
  delete NAME
             Delete a user specialist profile
  edit NAME  Open a user specialist profile in $VISUAL or $EDITOR
  path       Print specialist directories
  help       Show this help

Flags:
  --json                      Print JSON output
  --user                      Use the user specialist directory (default)
  --project                   Use the project .zero/specialists directory
  --description <text>        Description for create
  --prompt <text>             System prompt for create
  --tools <list>              Tool ids/categories for create
  --extends <name>            Base specialist for create
  --model <model>             Model override for create
  --reasoning-effort <value>  Reasoning effort override for create
  --force                     Replace an existing specialist during create
`)
	return err
}

func openEditor(path string) error {
	editor := strings.TrimSpace(os.Getenv("VISUAL"))
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if editor == "" {
		return fmt.Errorf("VISUAL or EDITOR must be set to edit specialists")
	}
	parts := strings.Fields(editor)
	if len(parts) == 0 {
		return fmt.Errorf("VISUAL or EDITOR must be set to edit specialists")
	}
	command := osexec.Command(parts[0], append(parts[1:], path)...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("editor failed: %w", err)
	}
	return nil
}
