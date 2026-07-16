package app

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
)

type app struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

type globalOptions struct {
	pinHome       string
	legacyPinHome string
	showVersion   bool
}

var errHelp = errors.New("help requested")

var version = "dev"

func RunCLI(args []string, stdout, stderr io.Writer) int {
	return runCLI(args, os.Stdin, stdout, stderr)
}

func runCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := app{stdin: stdin, stdout: stdout, stderr: stderr}
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
	if opts.showVersion {
		printVersion(a.stdout)
		return nil
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
	case "init":
		return a.commandInit(commandArgs)
	case "skill":
		return a.commandSkill(commandArgs)
	case "status":
		return a.commandWithOptionalContext(command, commandArgs, opts, a.commandStatus)
	case "verify":
		return a.commandWithOptionalContext(command, commandArgs, opts, a.commandVerify)
	case "check":
		return a.commandWithSourceContext(command, commandArgs, opts, a.commandCheck)
	case "update":
		return a.commandWithSourceContext(command, commandArgs, opts, a.commandUpdate)
	case "rollback":
		return a.commandWithOptionalContext(command, commandArgs, opts, a.commandRollback)
	case "run":
		toolOrPath, toolArgs, err := parseRunArgs(commandArgs)
		if err != nil {
			return err
		}
		ctx, err := resolveToolContext(toolOrPath, opts)
		if err != nil {
			return err
		}
		return a.commandRun(ctx, toolArgs)
	case "list":
		if len(commandArgs) != 0 {
			return fmt.Errorf("list takes no arguments")
		}
		return a.commandList(opts)
	case "version":
		if len(commandArgs) != 0 {
			return fmt.Errorf("version takes no arguments")
		}
		printVersion(a.stdout)
		return nil
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

func (a app) commandWithOptionalContext(command string, args []string, opts globalOptions, run func(pinContext) error) error {
	toolOrPath, hasArg, err := optionalSingleArg(command, args)
	if err != nil {
		return err
	}
	ctx, err := resolveContext(toolOrPath, hasArg, opts)
	if err != nil {
		return err
	}
	return run(ctx)
}

func (a app) commandWithSourceContext(command string, args []string, opts globalOptions, run func(pinContext) error) error {
	toolOrPath, hasArg, err := optionalSingleArg(command, args)
	if err != nil {
		return err
	}
	ctx, err := resolveSourceContext(toolOrPath, hasArg, opts)
	if err != nil {
		return err
	}
	return run(ctx)
}

func parseGlobalOptions(args []string, stdout io.Writer) (globalOptions, []string, error) {
	legacyHome := legacyPinHome()
	opts := globalOptions{
		pinHome: defaultPinHome(),
	}
	useLegacyFallback := os.Getenv("PIN_HOME") == ""

	flags := flag.NewFlagSet("pin", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.pinHome, "pin-home", opts.pinHome, "")
	flags.BoolVar(&opts.showVersion, "version", false, "")
	flags.Usage = func() { printUsage(stdout) }

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, nil, errHelp
		}
		return opts, nil, err
	}

	opts.pinHome = expandPath(opts.pinHome)
	pinHomeFromFlag := false
	flags.Visit(func(flag *flag.Flag) {
		if flag.Name == "pin-home" {
			pinHomeFromFlag = true
		}
	})
	if useLegacyFallback && !pinHomeFromFlag {
		opts.legacyPinHome = legacyHome
	}
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
	fmt.Fprintln(w, "Usage: pin [--pin-home PATH] [--version] <command> [tool_or_path]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  init [path]")
	fmt.Fprintln(w, "  skill <install|remove|status>")
	fmt.Fprintln(w, "  status [tool_or_path]")
	fmt.Fprintln(w, "  verify [tool_or_path]")
	fmt.Fprintln(w, "  check [tool_or_path]")
	fmt.Fprintln(w, "  update [tool_or_path]")
	fmt.Fprintln(w, "  rollback [tool_or_path]")
	fmt.Fprintln(w, "  run tool [-- args...]")
	fmt.Fprintln(w, "  list")
	fmt.Fprintln(w, "  version")
}

func printCommandUsage(w io.Writer, command string) {
	switch command {
	case "init":
		fmt.Fprintln(w, "Usage: pin init [--name NAME] [--source PATH] [--entrypoint NAME] [--requirements PATH] [--inject PATH] [--branch BRANCH] [--remote REMOTE] [--preflight COMMAND] [--verify COMMAND] [path]")
	case "skill":
		printSkillUsage(w)
	case "status", "verify", "check", "update", "rollback":
		fmt.Fprintf(w, "Usage: pin %s [tool_or_path]\n", command)
	case "run":
		fmt.Fprintln(w, "Usage: pin run tool [-- args...]")
	case "list":
		fmt.Fprintln(w, "Usage: pin list")
	case "version":
		fmt.Fprintln(w, "Usage: pin version")
	default:
		printUsage(w)
	}
}

func printVersion(w io.Writer) {
	fmt.Fprintf(w, "pin %s\n", Version())
}

func Version() string {
	if version != "" && version != "dev" {
		return strings.TrimPrefix(version, "v")
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			return strings.TrimPrefix(info.Main.Version, "v")
		}
	}
	return version
}

func isHelp(arg string) bool {
	return arg == "-h" || arg == "--help" || arg == "help"
}

func defaultPinHome() string {
	if value := os.Getenv("PIN_HOME"); value != "" {
		return expandPath(value)
	}
	legacy := legacyPinHome()
	if legacy == "" {
		return "."
	}
	return filepath.Join(legacy, "pin")
}

func legacyPinHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share")
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
	for _, path := range statusInjectedFiles(ctx, current) {
		fmt.Fprintf(w, "inject: %s\n", path)
	}
	if current != nil {
		fmt.Fprintf(w, "release: %s\n", current.string("git_sha"))
		fmt.Fprintf(w, "release_path: %s\n", releasePath(ctx, current.string("git_sha")))
		fmt.Fprintf(w, "source_path: %s\n", current.string("source_path"))
	} else if ctx.config != nil {
		fmt.Fprintf(w, "source_path: %s\n", ctx.config.sourcePath)
		fmt.Fprintf(w, "branch: %s\n", branchRef(*ctx.config))
	}
}

func statusInjectedFiles(ctx pinContext, current releaseMetadata) []string {
	if ctx.config != nil && len(ctx.config.inject) != 0 {
		if current != nil && current.schemaVersion() == 2 {
			sourcePath := current.string("source_path")
			if sourcePath == "" {
				sourcePath = ctx.config.sourcePath
			}
			if sourcePath == "" {
				return ctx.config.inject
			}
			return resolveSourcePaths(sourcePath, ctx.config.inject)
		}
		return resolveInjectedPaths(ctx, ctx.config.inject)
	}
	if current == nil {
		return nil
	}
	raw, ok := current["config"].(map[string]any)
	if !ok {
		return nil
	}
	inject, err := relativePathListFromMetadata(raw["inject"])
	if err != nil || len(inject) == 0 {
		return nil
	}
	if current.schemaVersion() == 2 {
		sourcePath := current.string("source_path")
		if sourcePath == "" {
			return inject
		}
		return resolveSourcePaths(sourcePath, inject)
	}
	return resolveInjectedPaths(ctx, inject)
}

func resolveSourcePaths(sourcePath string, paths []string) []string {
	resolved := make([]string, 0, len(paths))
	for _, path := range paths {
		resolved = append(resolved, filepath.Join(sourcePath, path))
	}
	return resolved
}

func resolveInjectedPaths(ctx pinContext, inject []string) []string {
	paths := make([]string, 0, len(inject))
	for _, path := range inject {
		paths = append(paths, ctx.sharedPath(path))
	}
	return paths
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

func parseRunArgs(args []string) (string, []string, error) {
	if len(args) == 0 {
		return "", nil, fmt.Errorf("run requires a tool")
	}
	if len(args) == 1 {
		return args[0], nil, nil
	}
	if args[1] != "--" {
		return "", nil, fmt.Errorf("run arguments must follow --")
	}
	return args[0], args[2:], nil
}

func (a app) commandRun(ctx pinContext, args []string) error {
	release, err := activeRelease(ctx)
	if err != nil {
		return err
	}
	metadata, err := readReleaseMetadata(release)
	if err != nil {
		return err
	}
	config, err := loadConfigFromMetadata(metadata)
	if err != nil {
		return err
	}
	if err := validateMetadata(ctx, release, metadata, *config); err != nil {
		return err
	}
	if err := verifyInjectedPaths(ctx, release, metadata, *config); err != nil {
		return err
	}
	entrypoint := filepath.Join(release, venvDir, "bin", metadata.string("entrypoint"))
	if err := requireFile(entrypoint, "missing entrypoint"); err != nil {
		return err
	}

	command := exec.Command(entrypoint, args...)
	command.Dir = release
	command.Env = entrypointEnv(entrypoint)
	command.Stdin = os.Stdin
	command.Stdout = a.stdout
	command.Stderr = a.stderr
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exitError{code: exit.ExitCode()}
		}
		return err
	}
	return nil
}

func (a app) commandList(opts globalOptions) error {
	seen := map[string]bool{}
	if err := a.writeCurrentList(opts, seen); err != nil {
		return err
	}
	if opts.legacyPinHome == "" {
		return nil
	}
	return a.writeLegacyList(opts.legacyPinHome, seen)
}

func (a app) writeCurrentList(opts globalOptions, seen map[string]bool) error {
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
		if seen[entry.Name()] {
			continue
		}
		ctx := pinContext{name: entry.Name(), pinHome: opts.pinHome}
		metadata, err := readCurrentMetadata(ctx)
		if err != nil {
			if _, ok, legacyErr := legacyContext(entry.Name(), opts); legacyErr == nil && ok {
				continue
			}
			return err
		}
		if metadata == nil {
			continue
		}
		seen[entry.Name()] = true
		fmt.Fprintf(a.stdout, "%s\t%s\t%s\t%s\n", ctx.name, metadata.string("git_sha"), ctx.toolRoot(), ctx.currentLink())
	}
	return nil
}

func (a app) writeLegacyList(pinHome string, seen map[string]bool) error {
	entries, err := os.ReadDir(pinHome)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || seen[entry.Name()] {
			continue
		}
		ctx := pinContext{name: entry.Name(), pinHome: pinHome}
		metadata, ok, err := legacyMetadata(ctx)
		if err != nil || !ok {
			continue
		}
		seen[entry.Name()] = true
		fmt.Fprintf(a.stdout, "%s\t%s\t%s\t%s\n", ctx.name, metadata.string("git_sha"), ctx.toolRoot(), ctx.currentLink())
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
