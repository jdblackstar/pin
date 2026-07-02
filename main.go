package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type app struct {
	stdout io.Writer
	stderr io.Writer
}

type globalOptions struct {
	pinHome string
	pinBin  string
}

var errHelp = errors.New("help requested")

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}

func runCLI(args []string, stdout, stderr io.Writer) int {
	a := app{stdout: stdout, stderr: stderr}
	if err := a.run(args); err != nil {
		if errors.Is(err, errHelp) {
			return 0
		}
		var exit exitError
		if errors.As(err, &exit) {
			return exit.code
		}
		fmt.Fprintf(stderr, "pin: error: %v\n", err)
		return 2
	}
	return 0
}

func (a app) run(args []string) error {
	opts, rest, err := parseGlobalOptions(args, a.stdout)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		printUsage(a.stdout)
		return errHelp
	}

	command := rest[0]
	commandArgs := rest[1:]
	if len(commandArgs) > 0 && isHelp(commandArgs[0]) {
		printCommandUsage(a.stdout, command)
		return errHelp
	}

	switch command {
	case "status", "verify", "check", "update", "rollback":
		toolOrPath, hasArg, err := optionalSingleArg(command, commandArgs)
		if err != nil {
			return err
		}
		ctx, err := resolveContext(toolOrPath, hasArg, opts.pinHome, opts.pinBin)
		if err != nil {
			return err
		}
		switch command {
		case "status":
			return a.commandStatus(ctx)
		case "verify":
			return a.commandVerify(ctx)
		case "check":
			return a.commandCheck(ctx)
		case "update":
			return a.commandUpdate(ctx)
		case "rollback":
			return a.commandRollback(ctx)
		}
	case "list":
		if len(commandArgs) != 0 {
			return fmt.Errorf("list takes no arguments")
		}
		return a.commandList(opts)
	default:
		return fmt.Errorf("unknown command %q", command)
	}

	return nil
}

func parseGlobalOptions(args []string, stdout io.Writer) (globalOptions, []string, error) {
	opts := globalOptions{
		pinHome: defaultPinHome(),
		pinBin:  defaultPinBin(),
	}

	flags := flag.NewFlagSet("pin", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.pinHome, "pin-home", opts.pinHome, "")
	flags.StringVar(&opts.pinBin, "pin-bin", opts.pinBin, "")
	flags.Usage = func() { printUsage(stdout) }

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(stdout)
			return opts, nil, errHelp
		}
		return opts, nil, err
	}

	opts.pinHome = expandPath(opts.pinHome)
	opts.pinBin = expandPath(opts.pinBin)
	return opts, flags.Args(), nil
}

func optionalSingleArg(command string, args []string) (string, bool, error) {
	if len(args) > 1 {
		return "", false, fmt.Errorf("%s takes at most one argument", command)
	}
	if len(args) == 0 {
		return "", false, nil
	}
	return args[0], true, nil
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: pin [--pin-home PATH] [--pin-bin PATH] <command> [tool_or_path]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  status [tool_or_path]")
	fmt.Fprintln(w, "  verify [tool_or_path]")
	fmt.Fprintln(w, "  check [tool_or_path]")
	fmt.Fprintln(w, "  update [tool_or_path]")
	fmt.Fprintln(w, "  rollback [tool_or_path]")
	fmt.Fprintln(w, "  list")
}

func printCommandUsage(w io.Writer, command string) {
	switch command {
	case "status", "verify", "check", "update", "rollback":
		fmt.Fprintf(w, "Usage: pin %s [tool_or_path]\n", command)
	case "list":
		fmt.Fprintln(w, "Usage: pin list")
	default:
		printUsage(w)
	}
}

func isHelp(arg string) bool {
	return arg == "-h" || arg == "--help" || arg == "help"
}

func defaultPinHome() string {
	if value := os.Getenv("PIN_HOME"); value != "" {
		return expandPath(value)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".local", "share")
}

func defaultPinBin() string {
	if value := os.Getenv("PIN_BIN"); value != "" {
		return expandPath(value)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".local", "bin")
}

func expandPath(path string) string {
	if path == "~" || len(path) > 2 && path[:2] == "~/" {
		if home, err := os.UserHomeDir(); err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, path[2:])
		}
	}
	return filepath.Clean(path)
}

func (a app) commandStatus(ctx pinContext) error {
	report, err := inspectStatus(ctx)
	if err != nil {
		return err
	}

	report.writeTo(a.stdout)
	return nil
}

type statusReport struct {
	ctx     pinContext
	current releaseMetadata
}

func inspectStatus(ctx pinContext) (statusReport, error) {
	current, err := readCurrentMetadata(ctx)
	return statusReport{ctx: ctx, current: current}, err
}

func (r statusReport) writeTo(w io.Writer) {
	ctx := r.ctx
	current := r.current

	fmt.Fprintf(w, "tool: %s\n", ctx.name)
	fmt.Fprintf(w, "installed: %s\n", yesNo(current != nil))
	fmt.Fprintf(w, "tool_root: %s\n", ctx.toolRoot())
	fmt.Fprintf(w, "current: %s\n", formatLink(ctx.currentLink()))
	fmt.Fprintf(w, "previous: %s\n", formatLink(ctx.previousLink()))
	fmt.Fprintf(w, "entrypoint: %s\n", ctx.stableEntrypoint())
	if current != nil {
		fmt.Fprintf(w, "release: %s\n", current.string("git_sha"))
		fmt.Fprintf(w, "release_path: %s\n", releasePath(ctx, current.string("git_sha")))
		fmt.Fprintf(w, "source_path: %s\n", current.string("source_path"))
	} else if ctx.config != nil {
		fmt.Fprintf(w, "source_path: %s\n", ctx.config.sourcePath)
		fmt.Fprintf(w, "branch: %s\n", branchRef(*ctx.config))
	}
}

func (a app) commandVerify(ctx pinContext) error {
	metadata, err := verifyActive(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "verified: %s %s\n", ctx.name, metadata.string("git_sha"))
	return nil
}

func (a app) commandCheck(ctx pinContext) error {
	report, err := checkRelease(ctx)
	if err != nil {
		return err
	}
	report.writeTo(a.stdout)
	if report.status != checkCurrent {
		return exitError{code: 1}
	}
	return nil
}

func (r checkReport) writeTo(w io.Writer) {
	fmt.Fprintf(w, "tool: %s\n", r.tool)
	if r.active == "" {
		fmt.Fprintf(w, "status: %s\n", r.status)
		fmt.Fprintf(w, "target: %s\n", r.target)
		return
	}
	fmt.Fprintf(w, "active: %s\n", r.active)
	fmt.Fprintf(w, "target: %s\n", r.target)
	fmt.Fprintf(w, "branch: %s\n", r.branch)
	fmt.Fprintf(w, "status: %s\n", r.status)
}

func (a app) commandUpdate(ctx pinContext) error {
	report, err := updateRelease(ctx)
	if err != nil {
		return err
	}

	fmt.Fprintf(a.stdout, "updated: %s %s\n", ctx.name, report.gitSHA)
	fmt.Fprintf(a.stdout, "current: %s -> %s\n", report.currentLink, report.currentTarget)
	fmt.Fprintf(a.stdout, "entrypoint: %s\n", report.entrypoint)
	return nil
}

func (a app) commandRollback(ctx pinContext) error {
	report, err := rollbackRelease(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "rolled back: %s %s\n", ctx.name, report.gitSHA)
	return nil
}

func (a app) commandList(opts globalOptions) error {
	entries, err := os.ReadDir(opts.pinHome)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		ctx := pinContext{name: entry.Name(), pinHome: opts.pinHome, pinBin: opts.pinBin}
		metadata, err := readCurrentMetadata(ctx)
		if err != nil {
			return err
		}
		if metadata == nil {
			continue
		}
		entrypoint := metadata.string("entrypoint")
		fmt.Fprintf(a.stdout, "%s\t%s\t%s\t%s\n", ctx.name, metadata.string("git_sha"), ctx.toolRoot(), filepath.Join(opts.pinBin, entrypoint))
	}
	return nil
}

type exitError struct {
	code int
}

func (e exitError) Error() string {
	return fmt.Sprintf("exit %d", e.code)
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
