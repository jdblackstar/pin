package pin

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type initOptions struct {
	name         string
	entrypoint   string
	source       string
	requirements string
	inject       repeatedString
	branch       string
	remote       string
	preflight    repeatedString
	verify       repeatedString
}

type repeatedString []string

func (values *repeatedString) String() string {
	return strings.Join(*values, ", ")
}

func (values *repeatedString) Set(value string) error {
	if value == "" {
		return fmt.Errorf("command cannot be empty")
	}
	*values = append(*values, value)
	return nil
}

func (a app) commandInit(args []string) error {
	opts, rest, err := parseInitOptions(args, a.stdout)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return fmt.Errorf("init takes at most one path")
	}

	sourcePath, err := initSourcePath(rest)
	if err != nil {
		return err
	}
	configPath := filepath.Join(sourcePath, configName)
	if pathExists(configPath) {
		return fmt.Errorf("%s already exists", configPath)
	}

	configText, err := renderInitConfig(opts, sourcePath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(configPath, []byte(configText), 0o644); err != nil {
		return err
	}

	fmt.Fprintf(a.stdout, "created: %s\n", configPath)
	return nil
}

func parseInitOptions(args []string, stdout io.Writer) (initOptions, []string, error) {
	opts := initOptions{
		remote: "origin",
	}

	flags := flag.NewFlagSet("pin init", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.name, "name", "", "")
	flags.StringVar(&opts.entrypoint, "entrypoint", "", "")
	flags.StringVar(&opts.source, "source", "", "")
	flags.StringVar(&opts.requirements, "requirements", "", "")
	flags.Var(&opts.inject, "inject", "")
	flags.StringVar(&opts.branch, "branch", "", "")
	flags.StringVar(&opts.remote, "remote", opts.remote, "")
	flags.Var(&opts.preflight, "preflight", "")
	flags.Var(&opts.verify, "verify", "")
	flags.Usage = func() { printCommandUsage(stdout, "init") }

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, nil, errHelp
		}
		return opts, nil, err
	}
	return opts, flags.Args(), nil
}

func initSourcePath(args []string) (string, error) {
	target := "."
	if len(args) == 1 {
		target = args[0]
	}
	target = expandPath(target)
	if err := requireDirectory(target, "source path"); err != nil {
		return "", err
	}
	return filepath.Abs(target)
}

func renderInitConfig(opts initOptions, sourcePath string) (string, error) {
	name := opts.name
	if name == "" {
		name = filepath.Base(sourcePath)
	}
	name, err := validatePathSegment(name, "pin.toml key \"name\"")
	if err != nil {
		return "", err
	}

	entrypoint := opts.entrypoint
	if entrypoint != "" {
		entrypoint, err = validatePathSegment(entrypoint, "pin.toml key \"entrypoint\"")
		if err != nil {
			return "", err
		}
	}
	source := opts.source
	if source == "" {
		source = "."
	}
	installSource, err := parseInstallSourceValue(source)
	if err != nil {
		return "", err
	}

	requirements, err := validateOptionalRelativeInitPath(opts.requirements, "requirements")
	if err != nil {
		return "", err
	}
	if requirements != "" && installSource.kind != sourceScript {
		return "", fmt.Errorf("%s key %q requires key %q to point to a Python script", configName, "requirements", "source")
	}
	inject, err := validateInjectPaths(opts.inject)
	if err != nil {
		return "", err
	}

	branch := opts.branch
	if branch == "" {
		branch = currentBranchOrDefault(sourcePath)
	}
	remote := opts.remote
	if remote == "" {
		return "", fmt.Errorf("%s key %q must be a non-empty string", configName, "remote")
	}

	verifyEntrypoint := name
	if entrypoint != "" {
		verifyEntrypoint = entrypoint
	}
	verify, err := initCommands(opts.verify, [][]string{{verifyEntrypoint, "--help"}}, "verify")
	if err != nil {
		return "", err
	}
	preflight, err := initCommands(opts.preflight, nil, "preflight")
	if err != nil {
		return "", err
	}

	config := initConfig{
		Name:       name,
		Branch:     branch,
		Remote:     remote,
		Preflight:  preflight,
		Verify:     verify,
		Entrypoint: entrypoint,
		Source:     installSource.path,
		Inject:     inject,
	}
	if entrypoint == name {
		config.Entrypoint = ""
	}
	if requirements != "" {
		config.Requirements = requirements
	}

	var b bytes.Buffer
	if err := toml.NewEncoder(&b).Encode(config); err != nil {
		return "", err
	}
	return b.String(), nil
}

type initConfig struct {
	Name         string     `toml:"name"`
	Source       string     `toml:"source"`
	Entrypoint   string     `toml:"entrypoint,omitempty"`
	Requirements string     `toml:"requirements,omitempty"`
	Inject       []string   `toml:"inject,omitempty"`
	Branch       string     `toml:"branch"`
	Remote       string     `toml:"remote"`
	Preflight    [][]string `toml:"preflight,omitempty"`
	Verify       [][]string `toml:"verify"`
}

func validateOptionalRelativeInitPath(value, key string) (string, error) {
	if value == "" {
		return "", nil
	}
	return validateRelativePath(value, fmt.Sprintf("%s key %q", configName, key))
}

func validateInjectPaths(values repeatedString) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	paths := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		path, err := validateRelativePath(value, fmt.Sprintf("%s key %q", configName, "inject"))
		if err != nil {
			return nil, err
		}
		if isReservedRuntimePath(path) {
			return nil, fmt.Errorf("%s key %q uses reserved runtime path %q", configName, "inject", path)
		}
		if seen[path] {
			return nil, fmt.Errorf("%s key %q contains duplicate path %q", configName, "inject", path)
		}
		seen[path] = true
		paths = append(paths, path)
	}
	return paths, nil
}

func currentBranchOrDefault(sourcePath string) string {
	branch, err := gitOutput(sourcePath, "branch", "--show-current")
	if err != nil || branch == "" {
		return "main"
	}
	return branch
}

func initCommands(raw repeatedString, fallback [][]string, key string) ([][]string, error) {
	if len(raw) == 0 {
		return fallback, nil
	}
	commands := make([][]string, 0, len(raw))
	for _, item := range raw {
		command, err := splitCommand(item)
		if err != nil || len(command) == 0 {
			return nil, fmt.Errorf("%s key %q contains an invalid command", configName, key)
		}
		commands = append(commands, command)
	}
	return commands, nil
}
