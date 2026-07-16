package app

import (
	"bufio"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

const (
	pinSkillName        = "pin"
	skillMarkerName     = "pin-managed.json"
	skillMarkerSchema   = 1
	skillTargetRelay    = "relay"
	skillTargetCodex    = "codex"
	skillTargetClaude   = "claude"
	skillTargetCursor   = "cursor"
	skillTargetOpenCode = "opencode"
)

//go:embed assets/pin-skill/SKILL.md
var embeddedPinSkill []byte

type skillMarker struct {
	Schema int    `json:"schema"`
	Skill  string `json:"skill"`
	Digest string `json:"digest"`
}

type skillInstallState string

const (
	skillMissing   skillInstallState = "missing"
	skillCurrent   skillInstallState = "current"
	skillOutdated  skillInstallState = "outdated"
	skillModified  skillInstallState = "modified"
	skillUnmanaged skillInstallState = "unmanaged"
)

type skillTarget struct {
	label    string
	path     string
	detected bool
}

type relaySkillConfig struct {
	configPath      string
	lockPath        string
	centralSkillDir string
	targetSkillDirs map[string]string
	enabledTools    map[string]bool
	relayExecutable string
}

type skillEnvironment struct {
	relay  *relaySkillConfig
	direct map[string]skillTarget
}

type skillMutationOptions struct {
	targets repeatedStringFlag
	yes     bool
	force   bool
}

type skillStatusOptions struct {
	targets repeatedStringFlag
}

func (a app) commandSkill(args []string) error {
	if len(args) == 0 {
		printSkillUsage(a.stdout)
		return errHelp
	}
	subcommand := args[0]
	subcommandArgs := args[1:]
	switch subcommand {
	case "install":
		return a.commandSkillInstall(subcommandArgs)
	case "remove":
		return a.commandSkillRemove(subcommandArgs)
	case "status":
		return a.commandSkillStatus(subcommandArgs)
	case "help", "-h", "--help":
		printSkillUsage(a.stdout)
		return errHelp
	default:
		return fmt.Errorf("unknown skill command %q", subcommand)
	}
}

func printSkillUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: pin skill <command> [options]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  install [--target TARGET] [--yes] [--force]")
	fmt.Fprintln(w, "  remove  [--target TARGET] [--yes] [--force]")
	fmt.Fprintln(w, "  status  [--target TARGET]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Targets: relay, codex, claude, cursor")
}

func printSkillCommandUsage(w io.Writer, command string) {
	switch command {
	case "install", "remove":
		fmt.Fprintf(w, "Usage: pin skill %s [--target TARGET] [--yes] [--force]\n", command)
	case "status":
		fmt.Fprintln(w, "Usage: pin skill status [--target TARGET]")
	default:
		printSkillUsage(w)
	}
}

func (a app) commandSkillInstall(args []string) error {
	opts, err := parseSkillMutationOptions("install", args, a.stdout)
	if err != nil {
		return err
	}
	env, err := detectSkillEnvironment()
	if err != nil {
		return err
	}
	targets, inferred, err := mutationSkillTargets(env, opts.targets)
	if err != nil {
		return err
	}
	if inferred && !opts.yes {
		confirmed, err := a.confirmSkillMutation("Install", targets, env)
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Fprintln(a.stdout, "cancelled")
			return nil
		}
	}

	for _, targetName := range targets {
		if targetName == skillTargetRelay {
			if err := a.installRelaySkill(env.relay, opts.force); err != nil {
				return err
			}
			continue
		}
		target := env.direct[targetName]
		changed, err := installSkillAt(target.path, opts.force)
		if err != nil {
			return fmt.Errorf("install %s skill: %w", targetName, err)
		}
		verb := "unchanged"
		if changed {
			verb = "installed"
		}
		fmt.Fprintf(a.stdout, "%s: %s %s\n", verb, targetName, target.path)
	}
	return nil
}

func (a app) commandSkillRemove(args []string) error {
	opts, err := parseSkillMutationOptions("remove", args, a.stdout)
	if err != nil {
		return err
	}
	env, err := detectSkillEnvironment()
	if err != nil {
		return err
	}
	targets, inferred, err := mutationSkillTargets(env, opts.targets)
	if err != nil {
		return err
	}
	if inferred && !opts.yes {
		confirmed, err := a.confirmSkillMutation("Remove", targets, env)
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Fprintln(a.stdout, "cancelled")
			return nil
		}
	}

	for _, targetName := range targets {
		if targetName == skillTargetRelay {
			removed, err := removeRelaySkill(env.relay, opts.force)
			if err != nil {
				return err
			}
			verb := "not installed"
			if removed {
				verb = "removed"
			}
			fmt.Fprintf(a.stdout, "%s: relay %s\n", verb, filepath.Join(env.relay.centralSkillDir, pinSkillName))
			continue
		}
		target := env.direct[targetName]
		removed, err := removeSkillAt(target.path, opts.force)
		if err != nil {
			return fmt.Errorf("remove %s skill: %w", targetName, err)
		}
		verb := "not installed"
		if removed {
			verb = "removed"
		}
		fmt.Fprintf(a.stdout, "%s: %s %s\n", verb, targetName, target.path)
	}
	return nil
}

func (a app) commandSkillStatus(args []string) error {
	opts, err := parseSkillStatusOptions(args, a.stdout)
	if err != nil {
		return err
	}
	env, err := detectSkillEnvironment()
	if err != nil {
		return err
	}
	targets, err := statusSkillTargets(opts.targets)
	if err != nil {
		return err
	}
	for _, targetName := range targets {
		if targetName == skillTargetRelay {
			if env.relay == nil {
				fmt.Fprintln(a.stdout, "relay: not configured")
				continue
			}
			path := filepath.Join(env.relay.centralSkillDir, pinSkillName)
			state, err := inspectSkillAt(path)
			if err != nil {
				return fmt.Errorf("inspect relay skill: %w", err)
			}
			fmt.Fprintf(a.stdout, "relay: %s path=%s distributes=%s\n", state, path, strings.Join(env.relay.enabledSkillTools(), ","))
			continue
		}
		target := env.direct[targetName]
		state, err := inspectSkillAt(target.path)
		if err != nil {
			return fmt.Errorf("inspect %s skill: %w", targetName, err)
		}
		fmt.Fprintf(a.stdout, "%s: %s detected=%s path=%s\n", targetName, state, yesNo(target.detected), target.path)
	}
	return nil
}

func parseSkillMutationOptions(command string, args []string, stdout io.Writer) (skillMutationOptions, error) {
	var opts skillMutationOptions
	flags := flag.NewFlagSet("pin skill "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Var(&opts.targets, "target", "")
	flags.BoolVar(&opts.yes, "yes", false, "")
	flags.BoolVar(&opts.force, "force", false, "")
	flags.Usage = func() { printSkillCommandUsage(stdout, command) }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, errHelp
		}
		return opts, err
	}
	if flags.NArg() != 0 {
		return opts, fmt.Errorf("skill %s takes no positional arguments", command)
	}
	return opts, nil
}

func parseSkillStatusOptions(args []string, stdout io.Writer) (skillStatusOptions, error) {
	var opts skillStatusOptions
	flags := flag.NewFlagSet("pin skill status", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Var(&opts.targets, "target", "")
	flags.Usage = func() { printSkillCommandUsage(stdout, "status") }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, errHelp
		}
		return opts, err
	}
	if flags.NArg() != 0 {
		return opts, fmt.Errorf("skill status takes no positional arguments")
	}
	return opts, nil
}

func mutationSkillTargets(env skillEnvironment, requested []string) ([]string, bool, error) {
	if len(requested) > 0 {
		targets, err := normalizeSkillTargets(requested)
		if err != nil {
			return nil, false, err
		}
		if containsString(targets, skillTargetRelay) && env.relay == nil {
			return nil, false, fmt.Errorf("Relay is not configured; run relay init or choose a direct target")
		}
		return targets, false, nil
	}
	if env.relay != nil {
		return []string{skillTargetRelay}, true, nil
	}
	var targets []string
	for _, name := range []string{skillTargetCodex, skillTargetClaude, skillTargetCursor} {
		if env.direct[name].detected {
			targets = append(targets, name)
		}
	}
	if len(targets) == 0 {
		return nil, false, fmt.Errorf("no supported agents detected; pass --target relay, codex, claude, or cursor")
	}
	return targets, true, nil
}

func statusSkillTargets(requested []string) ([]string, error) {
	if len(requested) > 0 {
		return normalizeSkillTargets(requested)
	}
	return []string{skillTargetRelay, skillTargetCodex, skillTargetClaude, skillTargetCursor}, nil
}

func normalizeSkillTargets(values []string) ([]string, error) {
	seen := make(map[string]bool)
	var targets []string
	for _, value := range values {
		name := strings.ToLower(strings.TrimSpace(value))
		if name == "claude-code" {
			name = skillTargetClaude
		}
		switch name {
		case skillTargetRelay, skillTargetCodex, skillTargetClaude, skillTargetCursor:
		default:
			return nil, fmt.Errorf("unknown skill target %q; expected relay, codex, claude, or cursor", value)
		}
		if !seen[name] {
			seen[name] = true
			targets = append(targets, name)
		}
	}
	return targets, nil
}

func (a app) confirmSkillMutation(action string, targets []string, env skillEnvironment) (bool, error) {
	if len(targets) == 1 && targets[0] == skillTargetRelay {
		fmt.Fprintf(a.stdout, "Detected Relay at %s. Relay will be the only default installation target.\n", env.relay.configPath)
		fmt.Fprintf(a.stdout, "%s the PIN skill through Relay? [Y/n] ", action)
	} else {
		labels := make([]string, 0, len(targets))
		for _, name := range targets {
			labels = append(labels, env.direct[name].label)
		}
		fmt.Fprintf(a.stdout, "Detected: %s.\n", strings.Join(labels, ", "))
		fmt.Fprintf(a.stdout, "%s the PIN skill for all detected agents? [Y/n] ", action)
	}
	reader := bufio.NewReader(a.stdin)
	answer, err := reader.ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	if err != nil && !(errors.Is(err, io.EOF) && answer != "") {
		if errors.Is(err, io.EOF) {
			return false, fmt.Errorf("confirmation required; rerun with --yes or an explicit --target")
		}
		return false, err
	}
	switch answer {
	case "", "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		return false, fmt.Errorf("expected yes or no, got %q", answer)
	}
}

func installSkillAt(path string, force bool) (bool, error) {
	state, err := inspectSkillAt(path)
	if err != nil {
		return false, err
	}
	switch state {
	case skillCurrent:
		return false, nil
	case skillModified, skillUnmanaged:
		if !force {
			return false, fmt.Errorf("%s skill already exists and is %s; inspect it or rerun with --force", path, state)
		}
	}

	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return false, err
	}
	tempRoot, err := os.MkdirTemp(parent, ".pin-skill-install-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tempRoot)
	candidate := filepath.Join(tempRoot, pinSkillName)
	if err := writeBundledSkill(candidate); err != nil {
		return false, err
	}

	backup := filepath.Join(tempRoot, "previous")
	if state != skillMissing {
		if err := os.Rename(path, backup); err != nil {
			return false, err
		}
	}
	if err := os.Rename(candidate, path); err != nil {
		if state != skillMissing {
			_ = os.Rename(backup, path)
		}
		return false, err
	}
	return true, nil
}

func removeSkillAt(path string, force bool) (bool, error) {
	state, err := inspectSkillAt(path)
	if err != nil {
		return false, err
	}
	if state == skillMissing {
		return false, nil
	}
	if (state == skillModified || state == skillUnmanaged) && !force {
		return false, fmt.Errorf("%s skill is %s; inspect it or rerun with --force", path, state)
	}
	return true, removeSkillPath(path)
}

func removeSkillPath(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return os.Remove(path)
	}
	parent := filepath.Dir(path)
	tempRoot, err := os.MkdirTemp(parent, ".pin-skill-remove-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempRoot)
	removed := filepath.Join(tempRoot, pinSkillName)
	if err := os.Rename(path, removed); err != nil {
		return err
	}
	return os.RemoveAll(removed)
}

func inspectSkillAt(path string) (skillInstallState, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return skillMissing, nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return skillUnmanaged, nil
	}
	skillData, err := os.ReadFile(filepath.Join(path, "SKILL.md"))
	if errors.Is(err, os.ErrNotExist) {
		return skillUnmanaged, nil
	}
	if err != nil {
		return "", err
	}
	markerData, err := os.ReadFile(filepath.Join(path, skillMarkerName))
	if errors.Is(err, os.ErrNotExist) {
		return skillUnmanaged, nil
	}
	if err != nil {
		return "", err
	}
	var marker skillMarker
	if err := json.Unmarshal(markerData, &marker); err != nil || marker.Schema != skillMarkerSchema || marker.Skill != pinSkillName || marker.Digest == "" {
		return skillUnmanaged, nil
	}
	currentDigest := skillDigest(skillData)
	if currentDigest == skillDigest(embeddedPinSkill) {
		return skillCurrent, nil
	}
	if currentDigest == marker.Digest {
		return skillOutdated, nil
	}
	return skillModified, nil
}

func writeBundledSkill(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(path, "SKILL.md"), embeddedPinSkill, 0o644); err != nil {
		return err
	}
	marker := skillMarker{Schema: skillMarkerSchema, Skill: pinSkillName, Digest: skillDigest(embeddedPinSkill)}
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(path, skillMarkerName), data, 0o644)
}

func skillDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (a app) installRelaySkill(relay *relaySkillConfig, force bool) error {
	if relay == nil {
		return fmt.Errorf("Relay is not configured")
	}
	if relay.relayExecutable == "" {
		return fmt.Errorf("Relay is configured at %s but the relay executable is not in PATH", relay.configPath)
	}
	lock, err := acquireRelaySkillLock(relay.lockPath)
	if err != nil {
		return fmt.Errorf("acquire Relay process lock: %w", err)
	}
	path := filepath.Join(relay.centralSkillDir, pinSkillName)
	changed, installErr := installSkillAt(path, force)
	lock.release()
	if installErr != nil {
		return fmt.Errorf("install Relay skill source: %w", installErr)
	}

	command := exec.Command(relay.relayExecutable, "sync")
	command.Stdout = a.stdout
	command.Stderr = a.stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("installed skill source at %s but Relay sync failed: %w", path, err)
	}
	verb := "unchanged"
	if changed {
		verb = "installed"
	}
	fmt.Fprintf(a.stdout, "%s: relay %s\n", verb, path)
	return nil
}

func removeRelaySkill(relay *relaySkillConfig, force bool) (bool, error) {
	if relay == nil {
		return false, fmt.Errorf("Relay is not configured")
	}
	lock, err := acquireRelaySkillLock(relay.lockPath)
	if err != nil {
		return false, fmt.Errorf("acquire Relay process lock: %w", err)
	}
	defer lock.release()

	paths := relay.managedSkillPaths()
	removed := false
	for _, path := range paths {
		state, err := inspectSkillAt(path)
		if err != nil {
			return false, err
		}
		if state == skillMissing {
			continue
		}
		if (state == skillModified || state == skillUnmanaged) && !force {
			return false, fmt.Errorf("Relay-managed skill at %s is %s; inspect it or rerun with --force", path, state)
		}
	}
	for _, path := range paths {
		state, err := inspectSkillAt(path)
		if err != nil {
			return false, err
		}
		if state == skillMissing {
			continue
		}
		if err := removeSkillPath(path); err != nil {
			return false, err
		}
		removed = true
	}
	return removed, nil
}

func (relay *relaySkillConfig) managedSkillPaths() []string {
	seen := make(map[string]bool)
	var paths []string
	add := func(base string) {
		if base == "" {
			return
		}
		path := filepath.Join(base, pinSkillName)
		if !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	add(relay.centralSkillDir)
	for _, tool := range relay.enabledSkillTools() {
		add(relay.targetSkillDirs[tool])
	}
	return paths
}

func (relay *relaySkillConfig) enabledSkillTools() []string {
	var tools []string
	for _, tool := range []string{skillTargetClaude, skillTargetCodex, skillTargetCursor, skillTargetOpenCode} {
		if relay.enabledTools[tool] && relay.targetSkillDirs[tool] != "" {
			tools = append(tools, tool)
		}
	}
	return tools
}

func detectSkillEnvironment() (skillEnvironment, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return skillEnvironment{}, fmt.Errorf("resolve home directory: %w", err)
	}
	relay, err := detectRelaySkillConfig(home)
	if err != nil {
		return skillEnvironment{}, err
	}
	codexHome := toolHome(home, "CODEX_HOME", ".codex")
	claudeHome := toolHome(home, "CLAUDE_HOME", ".claude")
	cursorHome := toolHome(home, "CURSOR_HOME", ".cursor")
	codexSkills := filepath.Join(home, ".agents", "skills")
	if value := strings.TrimSpace(os.Getenv("CODEX_HOME")); value != "" {
		codexSkills = filepath.Join(expandSkillPath(value, home), "skills")
	}
	codexSkillPath := filepath.Join(codexSkills, pinSkillName)
	claudeSkillPath := filepath.Join(claudeHome, "skills", pinSkillName)
	cursorSkillPath := filepath.Join(cursorHome, "skills", pinSkillName)
	direct := map[string]skillTarget{
		skillTargetCodex: {
			label:    "Codex",
			path:     codexSkillPath,
			detected: pathExists(codexHome) || pathExists(codexSkillPath) || executableExists("codex"),
		},
		skillTargetClaude: {
			label:    "Claude Code",
			path:     claudeSkillPath,
			detected: pathExists(claudeHome) || pathExists(claudeSkillPath) || executableExists("claude"),
		},
		skillTargetCursor: {
			label:    "Cursor",
			path:     cursorSkillPath,
			detected: pathExists(cursorHome) || pathExists(cursorSkillPath) || executableExists("cursor"),
		},
	}
	return skillEnvironment{relay: relay, direct: direct}, nil
}

func toolHome(home, envName, suffix string) string {
	if value := strings.TrimSpace(os.Getenv(envName)); value != "" {
		return expandSkillPath(value, home)
	}
	return filepath.Join(home, suffix)
}

func executableExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

type relayConfigFile struct {
	EnabledTools      []string `toml:"enabled_tools"`
	CentralSkillsDir  string   `toml:"central_skills_dir"`
	ClaudeSkillsDir   string   `toml:"claude_skills_dir"`
	CodexSkillsDir    string   `toml:"codex_skills_dir"`
	CursorSkillsDir   string   `toml:"cursor_skills_dir"`
	OpencodeSkillsDir string   `toml:"opencode_skills_dir"`
}

func detectRelaySkillConfig(home string) (*relaySkillConfig, error) {
	configPath := relayConfigPath(home)
	if configPath == "" {
		return nil, nil
	}
	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	defaults := relayConfigFile{
		EnabledTools:      []string{skillTargetClaude, skillTargetCodex, skillTargetCursor, skillTargetOpenCode},
		CentralSkillsDir:  filepath.Join(filepath.Dir(configPath), "skills"),
		ClaudeSkillsDir:   filepath.Join(toolHome(home, "CLAUDE_HOME", ".claude"), "skills"),
		CodexSkillsDir:    filepath.Join(toolHome(home, "CODEX_HOME", ".codex"), "skills"),
		OpencodeSkillsDir: filepath.Join(toolHome(home, "OPENCODE_HOME", ".config/opencode"), "skill"),
	}
	var raw relayConfigFile
	metadata, err := toml.DecodeFile(configPath, &raw)
	if err != nil {
		return nil, fmt.Errorf("read Relay config %s: %w", configPath, err)
	}
	if metadata.IsDefined("enabled_tools") {
		defaults.EnabledTools = raw.EnabledTools
	}
	pathFields := []struct {
		name        string
		destination *string
		source      string
	}{
		{"central_skills_dir", &defaults.CentralSkillsDir, raw.CentralSkillsDir},
		{"claude_skills_dir", &defaults.ClaudeSkillsDir, raw.ClaudeSkillsDir},
		{"codex_skills_dir", &defaults.CodexSkillsDir, raw.CodexSkillsDir},
		{"cursor_skills_dir", &defaults.CursorSkillsDir, raw.CursorSkillsDir},
		{"opencode_skills_dir", &defaults.OpencodeSkillsDir, raw.OpencodeSkillsDir},
	}
	for _, field := range pathFields {
		source := field.source
		if strings.TrimSpace(source) != "" {
			*field.destination = source
		}
		expanded, err := expandRelayConfigPath(*field.destination, home)
		if err != nil {
			return nil, fmt.Errorf("invalid %s in Relay config: %w", field.name, err)
		}
		*field.destination = expanded
	}

	enabled := make(map[string]bool)
	for _, tool := range defaults.EnabledTools {
		enabled[strings.ToLower(tool)] = true
	}
	relayExecutable, _ := exec.LookPath("relay")
	return &relaySkillConfig{
		configPath:      configPath,
		lockPath:        filepath.Join(filepath.Dir(configPath), "runtime", "relay.lock"),
		centralSkillDir: defaults.CentralSkillsDir,
		targetSkillDirs: map[string]string{
			skillTargetClaude:   defaults.ClaudeSkillsDir,
			skillTargetCodex:    defaults.CodexSkillsDir,
			skillTargetCursor:   defaults.CursorSkillsDir,
			skillTargetOpenCode: defaults.OpencodeSkillsDir,
		},
		enabledTools:    enabled,
		relayExecutable: relayExecutable,
	}, nil
}

func relayConfigPath(home string) string {
	if value := strings.TrimSpace(os.Getenv("RELAY_HOME")); value != "" {
		relayHome := expandSkillPath(value, home)
		return filepath.Join(relayHome, ".config", "relay", "config.toml")
	}
	xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if xdg == "" {
		xdg = filepath.Join(home, ".config")
	} else {
		xdg = expandSkillPath(xdg, home)
	}
	primary := filepath.Join(xdg, "relay", "config.toml")
	if pathExists(primary) {
		return primary
	}
	if configRoot, err := os.UserConfigDir(); err == nil {
		legacy := filepath.Join(configRoot, "relay", "config.toml")
		if legacy != primary && pathExists(legacy) {
			return legacy
		}
	}
	return primary
}

func expandRelayConfigPath(value, home string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if xdg == "" {
		xdg = filepath.Join(home, ".config")
	} else {
		xdg = expandSkillPath(xdg, home)
	}
	replacer := strings.NewReplacer(
		"${XDG_CONFIG_HOME:-$HOME/.config}", xdg,
		"${XDG_CONFIG_HOME:-${HOME}/.config}", xdg,
		"${XDG_CONFIG_HOME}", xdg,
		"$XDG_CONFIG_HOME", xdg,
		"${HOME}", home,
		"$HOME", home,
	)
	expanded := replacer.Replace(strings.TrimSpace(value))
	if strings.ContainsAny(expanded, "$`") {
		return "", fmt.Errorf("unsupported shell syntax in %q", value)
	}
	return expandSkillPath(expanded, home), nil
}

func expandSkillPath(value, home string) string {
	value = strings.TrimSpace(value)
	if value == "~" {
		return home
	}
	if strings.HasPrefix(value, "~/") {
		return filepath.Join(home, value[2:])
	}
	return filepath.Clean(value)
}
