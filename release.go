package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	metadataDir   = ".pin"
	metadataName  = "release.json"
	schemaVersion = 1
	checkCurrent  = "current"
)

type commandResult struct {
	args     []string
	cwd      string
	stdout   string
	stderr   string
	exitCode int
}

type releaseMetadata map[string]any

func (m releaseMetadata) string(key string) string {
	value, _ := m[key].(string)
	return value
}

func (m releaseMetadata) int(key string) (int, bool) {
	switch value := m[key].(type) {
	case int:
		return value, true
	case float64:
		return int(value), value == float64(int(value))
	case json.Number:
		parsed, err := strconv.Atoi(value.String())
		return parsed, err == nil
	default:
		return 0, false
	}
}

type checkReport struct {
	tool   string
	active string
	target string
	branch string
	status string
}

type updateReport struct {
	gitSHA        string
	currentLink   string
	currentTarget string
	entrypoint    string
}

type rollbackReport struct {
	gitSHA string
}

type releaseStep struct {
	name string
	run  func() error
}

func readMetadataForTool(pinHome, pinBin, name string) (releaseMetadata, error) {
	ctx := pinContext{name: name, pinHome: pinHome, pinBin: pinBin}
	return readCurrentMetadata(ctx)
}

func readCurrentMetadata(ctx pinContext) (releaseMetadata, error) {
	release, ok, err := releaseSymlink(ctx, ctx.currentLink())
	if err != nil || !ok {
		return nil, err
	}
	return readReleaseMetadata(release)
}

func releaseSymlink(ctx pinContext, link string) (string, bool, error) {
	info, err := os.Lstat(link)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", false, fmt.Errorf("%s exists but is not a symlink", link)
	}
	release, err := resolveReleaseLink(ctx, link)
	if err != nil {
		return "", false, err
	}
	return release, true, nil
}

func activeRelease(ctx pinContext) (string, error) {
	release, ok, err := releaseSymlink(ctx, ctx.currentLink())
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("missing active release: %s", ctx.currentLink())
	}
	return release, nil
}

func previousRelease(ctx pinContext) (string, error) {
	release, ok, err := releaseSymlink(ctx, ctx.previousLink())
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no previous release for %s", ctx.name)
	}
	return release, nil
}

func readReleaseMetadata(release string) (releaseMetadata, error) {
	path := filepath.Join(release, metadataDir, metadataName)
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("missing metadata: %s", path)
		}
		return nil, err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	decoder.UseNumber()
	var metadata map[string]any
	if err := decoder.Decode(&metadata); err != nil {
		return nil, fmt.Errorf("invalid metadata: %s: %w", path, err)
	}
	return releaseMetadata(metadata), nil
}

func writeReleaseMetadata(release string, config config, sha string) error {
	metadata := releaseMetadata{
		"schema_version": schemaVersion,
		"tool":           config.name,
		"entrypoint":     config.entrypoint,
		"git_sha":        sha,
		"source_path":    config.sourcePath,
		"branch":         config.branch,
		"remote":         config.remote,
		"installed_at":   time.Now().UTC().Format(time.RFC3339Nano),
		"release_path":   release,
		"config":         config.raw,
	}
	metadataPath := filepath.Join(release, metadataDir, metadataName)
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0o755); err != nil {
		return err
	}
	return atomicWriteJSON(metadataPath, metadata)
}

func atomicWriteJSON(path string, payload any) error {
	tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.%d.tmp", filepath.Base(path), os.Getpid()))
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(payload); err != nil {
		file.Close()
		os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func checkRelease(ctx pinContext) (checkReport, error) {
	config, err := requireConfig(ctx)
	if err != nil {
		return checkReport{}, err
	}
	targetSHA, err := fetchTargetSHA(*config)
	if err != nil {
		return checkReport{}, err
	}

	report := checkReport{
		tool:   ctx.name,
		target: targetSHA,
		branch: branchRef(*config),
		status: "not-installed",
	}

	current, err := readCurrentMetadata(ctx)
	if err != nil || current == nil {
		return report, err
	}

	report.active = current.string("git_sha")
	report.status, err = compareCommits(config.sourcePath, report.active, targetSHA)
	return report, err
}

func updateRelease(ctx pinContext) (updateReport, error) {
	config, err := requireConfig(ctx)
	if err != nil {
		return updateReport{}, err
	}
	sha, err := sourceSHAForUpdate(*config)
	if err != nil {
		return updateReport{}, err
	}
	release, err := ensureRelease(ctx, *config, sha)
	if err != nil {
		return updateReport{}, err
	}

	err = runSteps(
		releaseStep{"verify release", func() error { return verifyRelease(ctx, release, *config, false) }},
		releaseStep{"activate release", func() error { return activateRelease(ctx, sha) }},
		releaseStep{"verify active release", func() error {
			_, err := verifyActive(ctx)
			return err
		}},
		releaseStep{"link entrypoint", func() error {
			if !config.link {
				return nil
			}
			return ensureStableEntrypoint(ctx)
		}},
	)
	if err != nil {
		return updateReport{}, err
	}

	metadata, err := readCurrentMetadata(ctx)
	if err != nil {
		return updateReport{}, err
	}
	return updateReport{
		gitSHA:        metadata.string("git_sha"),
		currentLink:   ctx.currentLink(),
		currentTarget: readlink(ctx.currentLink()),
		entrypoint:    ctx.stableEntrypoint(),
	}, nil
}

func rollbackRelease(ctx pinContext) (rollbackReport, error) {
	previousTarget, err := previousRelease(ctx)
	if err != nil {
		return rollbackReport{}, err
	}
	config, err := configFromContextOrMetadata(ctx)
	if err != nil {
		return rollbackReport{}, err
	}
	previousSHA := filepath.Base(previousTarget)

	err = runSteps(
		releaseStep{"verify previous release", func() error { return verifyRelease(ctx, previousTarget, *config, false) }},
		releaseStep{"activate previous release", func() error { return activateRelease(ctx, previousSHA) }},
		releaseStep{"verify active release", func() error {
			_, err := verifyActive(ctx)
			return err
		}},
		releaseStep{"link entrypoint", func() error {
			if !config.link {
				return nil
			}
			return ensureStableEntrypoint(ctx)
		}},
	)
	if err != nil {
		return rollbackReport{}, err
	}

	metadata, err := readCurrentMetadata(ctx)
	if err != nil {
		return rollbackReport{}, err
	}
	return rollbackReport{gitSHA: metadata.string("git_sha")}, nil
}

func ensureRelease(ctx pinContext, config config, sha string) (string, error) {
	release := releasePath(ctx, sha)
	exists, err := directoryExists(release)
	if err != nil || exists {
		return release, err
	}
	return buildRelease(ctx, config, sha)
}

func sourceSHAForUpdate(config config) (string, error) {
	if err := ensureGitRepo(config.sourcePath); err != nil {
		return "", err
	}
	target, err := fetchTargetSHA(config)
	if err != nil {
		return "", err
	}
	branch, err := gitOutput(config.sourcePath, "branch", "--show-current")
	if err != nil {
		return "", err
	}
	if branch != config.branch {
		return "", fmt.Errorf("source is on branch %q, expected %q", branch, config.branch)
	}
	status, err := gitOutput(config.sourcePath, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if status != "" {
		return "", fmt.Errorf("source checkout is dirty")
	}
	head, err := gitOutput(config.sourcePath, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if head != target {
		return "", fmt.Errorf("HEAD %s does not match %s %s", head, branchRef(config), target)
	}
	if err := runConfiguredCommands(config.preflight, config.sourcePath, nil); err != nil {
		return "", err
	}
	return head, nil
}

func ensureGitRepo(path string) error {
	if _, err := gitOutput(path, "rev-parse", "--show-toplevel"); err != nil {
		return fmt.Errorf("%s is not a git repository", path)
	}
	return nil
}

func runSteps(steps ...releaseStep) error {
	for _, step := range steps {
		if err := step.run(); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}
	return nil
}

func directoryExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%s exists but is not a directory", path)
	}
	return true, nil
}

func buildRelease(ctx pinContext, config config, sha string) (string, error) {
	if err := os.MkdirAll(ctx.releasesDir(), 0o755); err != nil {
		return "", err
	}
	final := releasePath(ctx, sha)
	if exists, err := directoryExists(final); err != nil || exists {
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("release already exists: %s", final)
	}
	if err := os.MkdirAll(final, 0o755); err != nil {
		return "", err
	}

	source := filepath.Join(final, "src")
	if err := cleanupOnError(final, func() error {
		return runSteps(
			releaseStep{"create source directory", func() error { return os.Mkdir(source, 0o755) }},
			releaseStep{"extract source", func() error { return extractGitArchive(config.sourcePath, sha, source) }},
			releaseStep{"create virtualenv", func() error { return createVenv(final) }},
			releaseStep{"install source", func() error { return installSource(final, source) }},
			releaseStep{"write metadata", func() error { return writeReleaseMetadata(final, config, sha) }},
		)
	}); err != nil {
		return "", err
	}
	return final, nil
}

func cleanupOnError(path string, run func() error) error {
	if err := run(); err != nil {
		_ = os.RemoveAll(path)
		return err
	}
	return nil
}

func extractGitArchive(repo, sha, destination string) error {
	archive := exec.Command("git", "archive", "--format=tar", sha)
	archive.Dir = repo
	stdout, err := archive.StdoutPipe()
	if err != nil {
		return err
	}
	var archiveStderr bytes.Buffer
	archive.Stderr = &archiveStderr

	extract := exec.Command("tar", "-xf", "-", "-C", destination)
	extract.Stdin = stdout
	var extractStderr bytes.Buffer
	extract.Stderr = &extractStderr

	if err := archive.Start(); err != nil {
		return err
	}
	if err := extract.Start(); err != nil {
		archive.Wait()
		return err
	}
	extractErr := extract.Wait()
	archiveErr := archive.Wait()
	if archiveErr != nil {
		return fmt.Errorf("git archive failed: %s", strings.TrimSpace(archiveStderr.String()))
	}
	if extractErr != nil {
		return fmt.Errorf("tar extract failed: %s", strings.TrimSpace(extractStderr.String()))
	}
	return nil
}

func createVenv(release string) error {
	venvPath := filepath.Join(release, "venv")
	if uv, err := exec.LookPath("uv"); err == nil {
		_, err := runCommand([]string{uv, "venv", venvPath}, release, nil)
		return err
	}

	python, err := pythonCommand()
	if err != nil {
		return err
	}
	_, err = runCommand([]string{python, "-m", "venv", venvPath}, release, nil)
	return err
}

func installSource(release, source string) error {
	python := filepath.Join(release, "venv", "bin", "python")
	if uv, err := exec.LookPath("uv"); err == nil {
		_, err := runCommand([]string{uv, "pip", "install", "--python", python, "."}, source, nil)
		return err
	}

	_, err := runCommand([]string{python, "-m", "pip", "install", "."}, source, nil)
	return err
}

func pythonCommand() (string, error) {
	for _, name := range []string{"python3", "python"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("python3 or python is required to build releases")
}

func verifyActive(ctx pinContext) (releaseMetadata, error) {
	release, err := activeRelease(ctx)
	if err != nil {
		return nil, err
	}
	config, err := configFromContextOrMetadata(ctx)
	if err != nil {
		return nil, err
	}
	if err := verifyRelease(ctx, release, *config, true); err != nil {
		return nil, err
	}
	return readReleaseMetadata(release)
}

func verifyRelease(ctx pinContext, release string, config config, expectActive bool) error {
	if err := requireDirectory(release, "release"); err != nil {
		return err
	}
	metadata, err := readReleaseMetadata(release)
	if err != nil {
		return err
	}
	entrypoint := filepath.Join(release, "venv", "bin", metadata.string("entrypoint"))

	return runSteps(
		releaseStep{"validate metadata", func() error { return validateMetadata(ctx, release, metadata, config) }},
		releaseStep{"check entrypoint", func() error { return requireFile(entrypoint, "missing entrypoint") }},
		releaseStep{"check active link", func() error {
			if !expectActive {
				return nil
			}
			return requireActiveTarget(ctx, release)
		}},
		releaseStep{"run verify commands", func() error {
			return runConfiguredCommands(config.verify, filepath.Join(release, "src"), entrypointEnv(entrypoint))
		}},
	)
}

func requireDirectory(path, label string) error {
	exists, err := directoryExists(path)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%s does not exist: %s", label, path)
	}
	return nil
}

func requireFile(path, missingMessage string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: %s", missingMessage, path)
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}
	return nil
}

func requireActiveTarget(ctx pinContext, release string) error {
	currentTarget, err := activeRelease(ctx)
	if err != nil {
		return err
	}
	if currentTarget != release {
		return fmt.Errorf("active metadata does not match current link: %s != %s", currentTarget, release)
	}
	return nil
}

func entrypointEnv(entrypoint string) []string {
	return prependPath(os.Environ(), filepath.Dir(entrypoint))
}

func prependPath(env []string, dir string) []string {
	path := dir + string(os.PathListSeparator) + os.Getenv("PATH")
	updated := make([]string, 0, len(env)+1)
	replaced := false
	for _, item := range env {
		if strings.HasPrefix(item, "PATH=") {
			if !replaced {
				updated = append(updated, "PATH="+path)
				replaced = true
			}
			continue
		}
		updated = append(updated, item)
	}
	if !replaced {
		updated = append(updated, "PATH="+path)
	}
	return updated
}

func runConfiguredCommands(commands [][]string, cwd string, env []string) error {
	for _, command := range commands {
		if _, err := runCommand(command, cwd, env); err != nil {
			return err
		}
	}
	return nil
}

func validateMetadata(ctx pinContext, release string, metadata releaseMetadata, config config) error {
	if version, ok := metadata.int("schema_version"); !ok || version != schemaVersion {
		return fmt.Errorf("unsupported metadata schema: %v", metadata["schema_version"])
	}
	for _, key := range []string{"tool", "entrypoint", "git_sha", "source_path", "branch", "remote", "installed_at", "release_path"} {
		if metadata.string(key) == "" {
			return fmt.Errorf("metadata key %q is invalid in %s", key, release)
		}
	}
	if _, ok := metadata["config"].(map[string]any); !ok {
		return fmt.Errorf("metadata key %q is invalid in %s", "config", release)
	}
	if _, err := validatePathSegment(metadata.string("tool"), "metadata key 'tool'"); err != nil {
		return err
	}
	if _, err := validatePathSegment(metadata.string("entrypoint"), "metadata key 'entrypoint'"); err != nil {
		return err
	}
	if metadata.string("tool") != ctx.name {
		return fmt.Errorf("metadata tool %q does not match %q", metadata.string("tool"), ctx.name)
	}
	if metadata.string("git_sha") != filepath.Base(release) {
		return fmt.Errorf("metadata sha %s does not match release path %s", metadata.string("git_sha"), filepath.Base(release))
	}
	if filepath.Clean(metadata.string("release_path")) != filepath.Clean(release) {
		return fmt.Errorf("metadata release_path does not match release location")
	}
	if metadata.string("entrypoint") != config.entrypoint {
		return fmt.Errorf("metadata entrypoint does not match config")
	}
	if metadata.string("branch") != config.branch || metadata.string("remote") != config.remote {
		return fmt.Errorf("metadata branch/remote does not match config")
	}
	return nil
}

func activateRelease(ctx pinContext, sha string) error {
	target := releasePath(ctx, sha)
	exists, err := directoryExists(target)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("cannot activate missing release: %s", target)
	}

	oldTarget, hadOldTarget, err := releaseSymlink(ctx, ctx.currentLink())
	if err != nil {
		return err
	}
	if hadOldTarget && oldTarget != target {
		if err := replaceSymlink(ctx.previousLink(), filepath.Join("releases", filepath.Base(oldTarget))); err != nil {
			return err
		}
	}
	return replaceSymlink(ctx.currentLink(), filepath.Join("releases", sha))
}

func ensureStableEntrypoint(ctx pinContext) error {
	metadata, err := readCurrentMetadata(ctx)
	if err != nil {
		return err
	}
	if metadata == nil {
		return fmt.Errorf("cannot link entrypoint without active release for %s", ctx.name)
	}
	entrypoint := metadata.string("entrypoint")
	expected := filepath.Join(ctx.toolRoot(), "current", "venv", "bin", entrypoint)
	link := filepath.Join(ctx.pinBin, entrypoint)

	if err := ensureEntrypointCanBeReplaced(ctx, link); err != nil {
		return err
	}
	return replaceSymlink(link, expected)
}

func ensureEntrypointCanBeReplaced(ctx pinContext, link string) error {
	info, err := os.Lstat(link)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("entrypoint path exists and is not a symlink: %s", link)
	}

	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		return err
	}
	toolRoot, err := canonicalPath(ctx.toolRoot())
	if err != nil {
		return err
	}
	if !isRelativeTo(resolved, toolRoot) {
		return fmt.Errorf("entrypoint symlink points outside this tool: %s -> %s", link, readlink(link))
	}
	return nil
}

func canonicalPath(path string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved, nil
	}
	return filepath.Abs(path)
}

func replaceSymlink(path, target string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.%d.tmp", filepath.Base(path), os.Getpid()))
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func resolveReleaseLink(ctx pinContext, link string) (string, error) {
	target, err := os.Readlink(link)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", err
	}
	releases, err := filepath.Abs(ctx.releasesDir())
	if err != nil {
		return "", err
	}
	if !isRelativeTo(target, releases) {
		return "", fmt.Errorf("%s points outside releases: %s", link, target)
	}
	return target, nil
}

func releasePath(ctx pinContext, sha string) string {
	return filepath.Join(ctx.releasesDir(), sha)
}

func compareCommits(repo, active, target string) (string, error) {
	if active == target {
		return checkCurrent, nil
	}
	activeIsAncestor := gitOK(repo, "merge-base", "--is-ancestor", active, target)
	targetIsAncestor := gitOK(repo, "merge-base", "--is-ancestor", target, active)
	if activeIsAncestor {
		return "behind", nil
	}
	if targetIsAncestor {
		return "ahead", nil
	}
	return "diverged", nil
}

func branchRef(config config) string {
	return config.remote + "/" + config.branch
}

func fetchTargetSHA(config config) (string, error) {
	if _, err := runGit(config.sourcePath, "fetch", config.remote, config.branch); err != nil {
		return "", err
	}
	return gitOutput(config.sourcePath, "rev-parse", branchRef(config))
}

func gitOK(cwd string, args ...string) bool {
	command := exec.Command("git", args...)
	command.Dir = cwd
	return command.Run() == nil
}

func runGit(cwd string, args ...string) (commandResult, error) {
	return runCommand(append([]string{"git"}, args...), cwd, nil)
}

func gitOutput(cwd string, args ...string) (string, error) {
	result, err := runGit(cwd, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.stdout), nil
}

func runCommand(args []string, cwd string, env []string) (commandResult, error) {
	if len(args) == 0 {
		return commandResult{}, fmt.Errorf("empty command")
	}
	commandArgs := append([]string(nil), args...)
	if env != nil && !strings.ContainsRune(commandArgs[0], os.PathSeparator) {
		if resolved, ok := lookPathInEnv(commandArgs[0], env); ok {
			commandArgs[0] = resolved
		}
	}
	command := exec.Command(commandArgs[0], commandArgs[1:]...)
	command.Dir = cwd
	if env != nil {
		command.Env = env
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	err := command.Run()
	result := commandResult{
		args:   args,
		cwd:    cwd,
		stdout: stdout.String(),
		stderr: stderr.String(),
	}
	if command.ProcessState != nil {
		result.exitCode = command.ProcessState.ExitCode()
	}
	if err != nil {
		details := strings.TrimSpace(stderr.String())
		if details == "" {
			details = strings.TrimSpace(stdout.String())
		}
		suffix := ""
		if details != "" {
			suffix = ": " + details
		}
		return result, fmt.Errorf("command failed in %s: %s%s", cwd, quoteCommand(args), suffix)
	}
	return result, nil
}

func quoteCommand(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		if arg == "" || strings.ContainsAny(arg, " \t\n'\"\\") {
			quoted[i] = strconv.Quote(arg)
		} else {
			quoted[i] = arg
		}
	}
	return strings.Join(quoted, " ")
}

func lookPathInEnv(name string, env []string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], "PATH=") {
			for _, dir := range filepath.SplitList(strings.TrimPrefix(env[i], "PATH=")) {
				candidate := filepath.Join(dir, name)
				if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
					return candidate, true
				}
			}
			return "", false
		}
	}
	return "", false
}

func formatLink(path string) string {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return path + " (missing)"
		}
		return path + " (" + err.Error() + ")"
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return path + " -> " + readlink(path)
	}
	return path + " (not a symlink)"
}

func readlink(path string) string {
	target, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	return target
}

func isRelativeTo(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
