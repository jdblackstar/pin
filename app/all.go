package app

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

type scopedCommandOptions struct {
	all        bool
	yes        bool
	toolOrPath string
	hasArg     bool
}

func parseScopedCommandOptions(command string, args []string, allowYes bool) (scopedCommandOptions, error) {
	var opts scopedCommandOptions
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&opts.all, "all", false, "")
	if allowYes {
		flags.BoolVar(&opts.yes, "yes", false, "")
	}
	if err := flags.Parse(args); err != nil {
		return opts, err
	}
	remaining := flags.Args()
	if len(remaining) > 1 {
		return opts, fmt.Errorf("%s takes at most one tool or path", command)
	}
	if len(remaining) == 1 {
		opts.toolOrPath = remaining[0]
		opts.hasArg = true
	}
	if opts.all && opts.hasArg {
		return opts, fmt.Errorf("%s --all cannot be combined with a tool or path", command)
	}
	if opts.yes && !opts.all {
		return opts, fmt.Errorf("%s --yes requires --all", command)
	}
	return opts, nil
}

func (a app) commandVerifyArgs(args []string, opts globalOptions) error {
	commandOpts, err := parseScopedCommandOptions("verify", args, false)
	if err != nil {
		return err
	}
	if !commandOpts.all {
		ctx, err := resolveContext(commandOpts.toolOrPath, commandOpts.hasArg, opts)
		if err != nil {
			return err
		}
		return a.commandVerify(ctx)
	}
	return a.commandVerifyAll(opts)
}

func (a app) commandCheckArgs(args []string, opts globalOptions) error {
	commandOpts, err := parseScopedCommandOptions("check", args, false)
	if err != nil {
		return err
	}
	if !commandOpts.all {
		ctx, err := resolveSourceContext(commandOpts.toolOrPath, commandOpts.hasArg, opts)
		if err != nil {
			return err
		}
		return a.commandCheck(ctx)
	}
	return a.commandCheckAll(opts)
}

func (a app) commandUpdateArgs(args []string, opts globalOptions) error {
	commandOpts, err := parseScopedCommandOptions("update", args, true)
	if err != nil {
		return err
	}
	if !commandOpts.all {
		ctx, err := resolveSourceContext(commandOpts.toolOrPath, commandOpts.hasArg, opts)
		if err != nil {
			return err
		}
		return a.commandUpdate(ctx)
	}
	return a.commandUpdateAll(opts, commandOpts.yes)
}

func installedToolNames(opts globalOptions) ([]string, error) {
	names := map[string]bool{}
	entries, err := os.ReadDir(opts.pinHome)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			names[entry.Name()] = true
		}
	}

	if opts.legacyPinHome != "" {
		entries, err := os.ReadDir(opts.legacyPinHome)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() || names[entry.Name()] {
				continue
			}
			ctx := pinContext{name: entry.Name(), pinHome: opts.legacyPinHome}
			if _, ok, err := legacyMetadata(ctx); err == nil && ok {
				names[entry.Name()] = true
			}
		}
	}

	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

func (a app) commandVerifyAll(opts globalOptions) error {
	names, err := installedToolNames(opts)
	if err != nil {
		return err
	}
	passed, failed := 0, 0
	for _, name := range names {
		ctx, err := resolveToolContext(name, opts)
		var metadata releaseMetadata
		if err == nil {
			metadata, err = verifyActive(ctx)
		}
		if err != nil {
			failed++
			fmt.Fprintf(a.stderr, "failed: %s: %v\n", name, err)
			continue
		}
		passed++
		fmt.Fprintf(a.stdout, "verified: %s %s\n", name, metadata.string("git_sha"))
	}
	fmt.Fprintf(a.stdout, "summary: %d passed, %d failed\n", passed, failed)
	if failed != 0 {
		return exitError{code: 1}
	}
	return nil
}

func (a app) commandCheckAll(opts globalOptions) error {
	names, err := installedToolNames(opts)
	if err != nil {
		return err
	}
	current, nonCurrent, failed := 0, 0, 0
	for _, name := range names {
		ctx, err := resolveInstalledSourceContext(name, opts)
		var report checkReport
		if err == nil {
			report, err = checkRelease(ctx)
		}
		if err != nil {
			failed++
			fmt.Fprintf(a.stderr, "failed: %s: %v\n", name, err)
			continue
		}
		report.writeTo(a.stdout)
		if report.status == checkCurrent {
			current++
		} else {
			nonCurrent++
		}
	}
	fmt.Fprintf(a.stdout, "summary: %d current, %d non-current, %d failed\n", current, nonCurrent, failed)
	if nonCurrent != 0 || failed != 0 {
		return exitError{code: 1}
	}
	return nil
}

type plannedUpdate struct {
	ctx    pinContext
	report checkReport
}

func (a app) commandUpdateAll(opts globalOptions, yes bool) error {
	names, err := installedToolNames(opts)
	if err != nil {
		return err
	}
	planned := make([]plannedUpdate, 0)
	failed := 0
	for _, name := range names {
		ctx, err := resolveInstalledSourceContext(name, opts)
		var report checkReport
		if err == nil {
			report, err = checkRelease(ctx)
		}
		if err != nil {
			failed++
			fmt.Fprintf(a.stderr, "failed: %s: %v\n", name, err)
			continue
		}
		if report.status == checkCurrent {
			continue
		}
		planned = append(planned, plannedUpdate{ctx: ctx, report: report})
		fmt.Fprintf(a.stdout, "update available: %s (%s, %s -> %s)\n", name, report.status, displaySHA(report.active), displaySHA(report.target))
	}

	if len(planned) == 0 {
		if failed == 0 {
			fmt.Fprintln(a.stdout, "all tools are current")
			return nil
		}
		fmt.Fprintf(a.stdout, "summary: 0 updated, %d failed\n", failed)
		return exitError{code: 1}
	}

	if !yes {
		fmt.Fprintf(a.stdout, "Update %d %s? [y/N] ", len(planned), pluralizeTool(len(planned)))
		answer, _ := bufio.NewReader(a.stdin).ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(a.stdout, "update cancelled")
			if failed != 0 {
				return exitError{code: 1}
			}
			return nil
		}
	}

	updated := 0
	for _, candidate := range planned {
		report, err := updateRelease(candidate.ctx)
		if err != nil {
			failed++
			fmt.Fprintf(a.stderr, "failed: %s: %v\n", candidate.ctx.name, err)
			continue
		}
		updated++
		fmt.Fprintf(a.stdout, "updated: %s %s\n", candidate.ctx.name, report.gitSHA)
		fmt.Fprintf(a.stdout, "current: %s -> %s\n", report.currentLink, report.currentTarget)
	}
	fmt.Fprintf(a.stdout, "summary: %d updated, %d failed\n", updated, failed)
	if failed != 0 {
		return exitError{code: 1}
	}
	return nil
}

func displaySHA(sha string) string {
	if sha == "" {
		return "not-installed"
	}
	return sha
}

func pluralizeTool(count int) string {
	if count == 1 {
		return "tool"
	}
	return "tools"
}
