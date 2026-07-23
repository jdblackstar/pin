package app

import (
	"bufio"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	pinSkillName                   = "pin"
	skillMarkerName                = "pin-managed.json"
	skillMarkerSchema              = 2
	legacySkillMarkerSchema        = 1
	relayCapabilitiesSchema        = 1
	relayScopedSkillSyncCapability = "skills.sync.scoped"
	relayScopedSkillSyncVersion    = 1
	relayCapabilitiesTimeout       = 2 * time.Second
	relayScopedSkillSyncTimeout    = 30 * time.Second
	relayCommandWaitDelay          = 250 * time.Millisecond
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
	path     string
	detected bool
}

type skillEnvironment struct {
	canonical      skillTarget
	claude         skillTarget
	allowRelaySync bool
}

type skillMutationOptions struct {
	yes   bool
	force bool
}

type relayCapabilities struct {
	SchemaVersion int            `json:"schema_version"`
	Capabilities  map[string]int `json:"capabilities"`
}

// relayAdapter is the only process boundary between PIN and Relay. Keep all
// capability negotiation and subprocess argv here so Relay's optional contract
// can evolve without leaking into skill lifecycle code.
type relayAdapter struct {
	executable string
}

func findRelayAdapter() (relayAdapter, bool) {
	executable, err := exec.LookPath("relay")
	if err != nil {
		return relayAdapter{}, false
	}
	return relayAdapter{executable: executable}, true
}

func (relay relayAdapter) supportsScopedSkillSync() bool {
	output, err := relay.run(relayCapabilitiesTimeout, "capabilities", "--json")
	if err != nil {
		return false
	}
	var capabilities relayCapabilities
	if err := json.Unmarshal(output, &capabilities); err != nil {
		return false
	}
	return capabilities.SchemaVersion == relayCapabilitiesSchema &&
		capabilities.Capabilities[relayScopedSkillSyncCapability] >= relayScopedSkillSyncVersion
}

func (relay relayAdapter) syncSkill(path string) error {
	_, err := relay.run(
		relayScopedSkillSyncTimeout,
		"sync", "skill", "--quiet", "--fail-on-conflict", path,
	)
	if err != nil {
		return fmt.Errorf("scoped skill sync: %w", err)
	}
	return nil
}

func (relay relayAdapter) run(timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, relay.executable, args...)
	command.WaitDelay = relayCommandWaitDelay
	output, err := command.Output()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("relay command timed out")
	}
	return output, err
}

func printSkillUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: pin skill <command> [options]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  install [--yes] [--force]")
	fmt.Fprintln(w, "  remove  [--yes] [--force]")
	fmt.Fprintln(w, "  status")
}

func printSkillCommandUsage(w io.Writer, command string) {
	switch command {
	case "install", "remove":
		fmt.Fprintf(w, "Usage: pin skill %s [--yes] [--force]\n", command)
	case "status":
		fmt.Fprintln(w, "Usage: pin skill status")
	default:
		printSkillUsage(w)
	}
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

func (a app) commandSkillInstall(args []string) error {
	opts, err := parseSkillMutationOptions("install", args, a.stdout)
	if err != nil {
		return err
	}
	env, err := detectSkillEnvironment()
	if err != nil {
		return err
	}
	if !opts.yes {
		prompt := fmt.Sprintf("Install the PIN skill at %s? [Y/n] ", env.canonical.path)
		if env.claude.detected {
			prompt = fmt.Sprintf(
				"Install the PIN skill at %s and, if needed, a Claude Code compatibility copy at %s? [Y/n] ",
				env.canonical.path,
				env.claude.path,
			)
		}
		confirmed, err := a.confirmSkillMutation(
			prompt,
			true,
		)
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Fprintln(a.stdout, "cancelled")
			return nil
		}
	}

	changed, err := installSkillAt(env.canonical.path, opts.force)
	if err != nil {
		return fmt.Errorf("install canonical skill: %w", err)
	}
	printSkillInstallResult(a.stdout, changed, "skill", env.canonical.path)

	var relaySyncErr error
	if env.allowRelaySync {
		if relay, found := findRelayAdapter(); found && relay.supportsScopedSkillSync() {
			relaySyncErr = relay.syncSkill(env.canonical.path)
			if relaySyncErr == nil {
				return nil
			}
		}
	}

	fallbackUsed := false
	if env.claude.detected {
		fallbackChanged, fallbackErr := installSkillAt(env.claude.path, opts.force)
		if fallbackErr != nil {
			if relaySyncErr != nil {
				return fmt.Errorf(
					"canonical skill remains installed, but Relay sync failed (%v) and the Claude Code compatibility copy could not be installed: %w",
					relaySyncErr,
					fallbackErr,
				)
			}
			return fmt.Errorf("canonical skill remains installed, but install Claude Code compatibility copy: %w", fallbackErr)
		}
		fallbackUsed = true
		printSkillInstallResult(a.stdout, fallbackChanged, "compatibility copy", env.claude.path)
	}
	if relaySyncErr != nil {
		if fallbackUsed {
			fmt.Fprintf(a.stderr, "pin: warning: Relay could not synchronize compatibility copies; used the standalone fallback: %v\n", relaySyncErr)
		} else {
			fmt.Fprintf(a.stderr, "pin: warning: Relay could not synchronize compatibility copies; the canonical skill remains installed: %v\n", relaySyncErr)
		}
	}
	return nil
}

func printSkillInstallResult(w io.Writer, changed bool, kind, path string) {
	verb := "unchanged"
	if changed {
		verb = "installed"
	}
	fmt.Fprintf(w, "%s %s: %s\n", verb, kind, path)
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
	compatibilityCopy := env.claude.detected || pathExists(env.claude.path)
	if !opts.yes {
		prompt := fmt.Sprintf("Remove the PIN skill at %s? [y/N] ", env.canonical.path)
		if compatibilityCopy {
			prompt = fmt.Sprintf(
				"Remove the PIN skill at %s and its standard compatibility copy? [y/N] ",
				env.canonical.path,
			)
		}
		confirmed, err := a.confirmSkillMutation(
			prompt,
			false,
		)
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Fprintln(a.stdout, "cancelled")
			return nil
		}
	}

	targets := []skillTarget{env.canonical}
	if pathExists(env.claude.path) {
		targets = append(targets, env.claude)
	}
	removed, removeErr := removeSkillTargets(targets, opts.force)
	for _, path := range removed {
		fmt.Fprintf(a.stdout, "removed skill: %s\n", path)
	}
	return removeErr
}

func (a app) commandSkillStatus(args []string) error {
	if err := parseSkillStatusArgs(args, a.stdout); err != nil {
		return err
	}
	env, err := detectSkillEnvironment()
	if err != nil {
		return err
	}
	state, err := inspectSkillAt(env.canonical.path)
	if err != nil {
		return fmt.Errorf("inspect canonical skill: %w", err)
	}
	fmt.Fprintf(a.stdout, "pin: %s path=%s\n", state, env.canonical.path)
	if env.claude.detected || pathExists(env.claude.path) {
		state, err := inspectSkillAt(env.claude.path)
		if err != nil {
			return fmt.Errorf("inspect Claude Code compatibility copy: %w", err)
		}
		fmt.Fprintf(a.stdout, "claude compatibility: %s detected=%s path=%s\n", state, yesNo(env.claude.detected), env.claude.path)
	}
	return nil
}

func parseSkillMutationOptions(command string, args []string, stdout io.Writer) (skillMutationOptions, error) {
	var opts skillMutationOptions
	flags := flag.NewFlagSet("pin skill "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
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

func parseSkillStatusArgs(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("pin skill status", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() { printSkillCommandUsage(stdout, "status") }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return errHelp
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("skill status takes no positional arguments")
	}
	return nil
}

func (a app) confirmSkillMutation(prompt string, defaultYes bool) (bool, error) {
	fmt.Fprint(a.stdout, prompt)
	reader := bufio.NewReader(a.stdin)
	answer, err := reader.ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	if err != nil && !(errors.Is(err, io.EOF) && answer != "") {
		if errors.Is(err, io.EOF) {
			return false, fmt.Errorf("confirmation required; rerun with --yes")
		}
		return false, err
	}
	if answer == "" {
		return defaultYes, nil
	}
	switch answer {
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		return false, fmt.Errorf("expected yes or no, got %q", answer)
	}
}

func detectSkillEnvironment() (skillEnvironment, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return skillEnvironment{}, fmt.Errorf("resolve home directory: %w", err)
	}
	skillHome := home
	allowRelaySync := true
	skillHomeOverridden := false
	if value := strings.TrimSpace(os.Getenv("PIN_SKILL_HOME")); value != "" {
		skillHome = expandSkillPath(value, home)
		skillHomeOverridden = true
		// A skill-home override is an isolated development boundary. Relay's
		// provider configuration is independent and may point back at live
		// locations, so use PIN's standalone compatibility handling here.
		allowRelaySync = false
	}
	canonicalPath := filepath.Join(skillHome, ".agents", "skills", pinSkillName)
	claudeConfigSet := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")) != ""
	detectedClaudeHome := toolHome(home, "CLAUDE_CONFIG_DIR", ".claude")
	claudeHome := detectedClaudeHome
	if skillHomeOverridden {
		claudeHome = filepath.Join(skillHome, ".claude")
	}
	claudePath := filepath.Join(claudeHome, "skills", pinSkillName)
	return skillEnvironment{
		canonical: skillTarget{path: canonicalPath, detected: true},
		claude: skillTarget{
			path:     claudePath,
			detected: claudeConfigSet || pathExists(detectedClaudeHome) || executableExists("claude"),
		},
		allowRelaySync: allowRelaySync,
	}, nil
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

func installSkillAt(path string, force bool) (bool, error) {
	return installSkillAtWithRename(path, force, os.Rename)
}

func installSkillAtWithRename(path string, force bool, rename func(string, string) error) (bool, error) {
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
	cleanupTemp := true
	defer func() {
		if cleanupTemp {
			_ = os.RemoveAll(tempRoot)
		}
	}()
	candidate := filepath.Join(tempRoot, pinSkillName)
	if err := writeBundledSkill(candidate); err != nil {
		return false, err
	}

	backup := filepath.Join(tempRoot, "previous")
	if state != skillMissing {
		if err := rename(path, backup); err != nil {
			return false, err
		}
	}
	if err := rename(candidate, path); err != nil {
		if state != skillMissing {
			if restoreErr := rename(backup, path); restoreErr != nil {
				cleanupTemp = false
				return false, fmt.Errorf(
					"publish skill: %w; restore previous skill: %v; previous skill preserved at %s",
					err,
					restoreErr,
					backup,
				)
			}
		}
		return false, fmt.Errorf("publish skill: %w", err)
	}
	return true, nil
}

func removeSkillAt(path string, force bool) (bool, error) {
	removed, err := removeSkillTargets([]skillTarget{{path: path}}, force)
	return len(removed) != 0, err
}

type stagedSkillRemoval struct {
	originalPath string
	tempRoot     string
	stagedPath   string
}

func removeSkillTargets(targets []skillTarget, force bool) ([]string, error) {
	return removeSkillTargetsWithOps(targets, force, os.Rename, os.RemoveAll)
}

func removeSkillTargetsWithOps(
	targets []skillTarget,
	force bool,
	rename func(string, string) error,
	removeAll func(string) error,
) ([]string, error) {
	for _, target := range targets {
		state, err := inspectSkillAt(target.path)
		if err != nil {
			return nil, err
		}
		if (state == skillModified || state == skillUnmanaged) && !force {
			return nil, fmt.Errorf("%s skill is %s; inspect it or rerun with --force", target.path, state)
		}
	}

	var staged []stagedSkillRemoval
	rollback := func(cause error) ([]string, error) {
		if rollbackErr := rollbackSkillRemovals(staged, rename, removeAll); rollbackErr != nil {
			return nil, errors.Join(cause, rollbackErr)
		}
		return nil, cause
	}
	for _, target := range targets {
		removal, err := stageSkillRemoval(target.path, rename, removeAll)
		if err != nil {
			return rollback(fmt.Errorf("stage removal of %s: %w", target.path, err))
		}
		if removal == nil {
			continue
		}
		staged = append(staged, *removal)

		state, err := inspectSkillAt(removal.stagedPath)
		if err != nil {
			return rollback(fmt.Errorf("inspect staged skill %s: %w", target.path, err))
		}
		if (state == skillModified || state == skillUnmanaged) && !force {
			return rollback(fmt.Errorf("%s skill became %s during removal; no skills were removed", target.path, state))
		}
	}

	removed := make([]string, 0, len(staged))
	var cleanupErrors []error
	for _, removal := range staged {
		removed = append(removed, removal.originalPath)
		if err := removeAll(removal.tempRoot); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf(
				"%s was removed, but cleanup failed; recovery copy remains at %s: %w",
				removal.originalPath,
				removal.stagedPath,
				err,
			))
		}
	}
	return removed, errors.Join(cleanupErrors...)
}

func stageSkillRemoval(
	path string,
	rename func(string, string) error,
	removeAll func(string) error,
) (*stagedSkillRemoval, error) {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	tempRoot, err := os.MkdirTemp(filepath.Dir(path), ".pin-skill-remove-")
	if err != nil {
		return nil, err
	}
	stagedPath := filepath.Join(tempRoot, pinSkillName)
	if err := rename(path, stagedPath); err != nil {
		_ = removeAll(tempRoot)
		return nil, err
	}
	return &stagedSkillRemoval{
		originalPath: path,
		tempRoot:     tempRoot,
		stagedPath:   stagedPath,
	}, nil
}

func rollbackSkillRemovals(
	staged []stagedSkillRemoval,
	rename func(string, string) error,
	removeAll func(string) error,
) error {
	var rollbackErrors []error
	for i := len(staged) - 1; i >= 0; i-- {
		removal := staged[i]
		if err := rename(removal.stagedPath, removal.originalPath); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf(
				"restore %s failed; recovery copy remains at %s: %w",
				removal.originalPath,
				removal.stagedPath,
				err,
			))
			continue
		}
		if err := removeAll(removal.tempRoot); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("clean rollback directory %s: %w", removal.tempRoot, err))
		}
	}
	return errors.Join(rollbackErrors...)
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
	if err := json.Unmarshal(markerData, &marker); err != nil || marker.Skill != pinSkillName || marker.Digest == "" {
		return skillUnmanaged, nil
	}
	switch marker.Schema {
	case legacySkillMarkerSchema:
		onlyLegacyContents, err := hasOnlyLegacyManagedContents(path)
		if err != nil {
			return "", err
		}
		if !onlyLegacyContents {
			return skillModified, nil
		}
		if skillDigest(skillData) == marker.Digest {
			return skillOutdated, nil
		}
		return skillModified, nil
	case skillMarkerSchema:
		currentDigest, err := skillPackageDigest(path)
		if errors.Is(err, errUnsupportedSkillEntry) {
			return skillModified, nil
		}
		if err != nil {
			return "", err
		}
		if currentDigest == bundledSkillDigest() {
			return skillCurrent, nil
		}
		if currentDigest == marker.Digest {
			return skillOutdated, nil
		}
		return skillModified, nil
	default:
		return skillUnmanaged, nil
	}
}

func writeBundledSkill(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(path, "SKILL.md"), embeddedPinSkill, 0o644); err != nil {
		return err
	}
	marker := skillMarker{Schema: skillMarkerSchema, Skill: pinSkillName, Digest: bundledSkillDigest()}
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

var errUnsupportedSkillEntry = errors.New("unsupported skill package entry")

func bundledSkillDigest() string {
	hasher := sha256.New()
	writeSkillDigestEntry(hasher, "file", "SKILL.md", embeddedPinSkill)
	return hex.EncodeToString(hasher.Sum(nil))
}

func skillPackageDigest(root string) (string, error) {
	hasher := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." || relative == skillMarkerName {
			return nil
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			writeSkillDigestEntry(hasher, "dir", relative, nil)
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("%w: %s", errUnsupportedSkillEntry, relative)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		writeSkillDigestEntry(hasher, "file", relative, data)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func writeSkillDigestEntry(hasher hash.Hash, kind, path string, data []byte) {
	fmt.Fprintf(hasher, "%s\x00%d\x00%s\x00%d\x00", kind, len(path), path, len(data))
	_, _ = hasher.Write(data)
	_, _ = hasher.Write([]byte{0})
}

func hasOnlyLegacyManagedContents(root string) (bool, error) {
	seenSkill := false
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		switch relative {
		case ".", skillMarkerName:
			return nil
		case "SKILL.md":
			if !entry.Type().IsRegular() {
				return errUnsupportedSkillEntry
			}
			seenSkill = true
			return nil
		default:
			return errUnsupportedSkillEntry
		}
	})
	if errors.Is(err, errUnsupportedSkillEntry) {
		return false, nil
	}
	return seenSkill, err
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
