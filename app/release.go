package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	metadataDir               = ".pin"
	metadataName              = "release.json"
	integrityName             = "integrity.json"
	venvDir                   = ".venv"
	schemaVersion             = 3
	checkCurrent              = "current"
	releaseCommandTimeout     = 15 * time.Minute
	releaseCommandOutputLimit = 1 << 20
	releaseCommandWaitDelay   = time.Second
	truncatedOutputMarker     = "[... output truncated ...]\n"
)

type commandLimits struct {
	timeout     time.Duration
	outputLimit int
}

var defaultCommandLimits = commandLimits{
	timeout:     releaseCommandTimeout,
	outputLimit: releaseCommandOutputLimit,
}

const integrityAlgorithm = "sha256"

type commandResult struct {
	args     []string
	cwd      string
	stdout   string
	stderr   string
	exitCode int
}

type boundedBuffer struct {
	data      []byte
	limit     int
	truncated bool
}

// newBoundedBuffer creates a buffer that retains at most the newest limit bytes.
func newBoundedBuffer(limit int) *boundedBuffer {
	if limit < 0 {
		limit = 0
	}
	return &boundedBuffer{data: make([]byte, 0, limit), limit: limit}
}

// Write appends data while discarding the oldest bytes beyond the buffer limit.
func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	written := len(data)
	if written == 0 {
		return 0, nil
	}
	if buffer.limit == 0 {
		buffer.truncated = true
		return written, nil
	}
	if len(data) > buffer.limit {
		buffer.data = buffer.data[:buffer.limit]
		copy(buffer.data, data[len(data)-buffer.limit:])
		buffer.truncated = true
		return written, nil
	}
	if overflow := len(buffer.data) + len(data) - buffer.limit; overflow > 0 {
		copy(buffer.data, buffer.data[overflow:])
		buffer.data = buffer.data[:len(buffer.data)-overflow]
		buffer.truncated = true
	}
	buffer.data = append(buffer.data, data...)
	return written, nil
}

// String returns retained output with a marker when earlier bytes were discarded.
func (buffer *boundedBuffer) String() string {
	if !buffer.truncated {
		return string(buffer.data)
	}
	if buffer.limit <= len(truncatedOutputMarker) {
		return truncatedOutputMarker[:buffer.limit]
	}
	retained := buffer.limit - len(truncatedOutputMarker)
	return truncatedOutputMarker + string(buffer.data[len(buffer.data)-retained:])
}

type releaseMetadata map[string]any

type integrityReference struct {
	Algorithm      string `json:"algorithm"`
	ManifestSHA256 string `json:"manifest_sha256"`
}

type integrityManifest struct {
	Version        int              `json:"version"`
	MetadataSHA256 string           `json:"metadata_sha256"`
	Entries        []integrityEntry `json:"entries"`
}

type integrityEntry struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Mode   string `json:"mode"`
	SHA256 string `json:"sha256,omitempty"`
}

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

func (m releaseMetadata) schemaVersion() int {
	version, _ := m.int("schema_version")
	return version
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
}

type rollbackReport struct {
	gitSHA string
}

type releaseStep struct {
	name string
	run  func() error
}

func readMetadataForTool(pinHome, name string) (releaseMetadata, error) {
	ctx := pinContext{name: name, pinHome: pinHome}
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
	file, err := openRegularFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("missing metadata: %s", path)
		}
		if errors.Is(err, errNotRegularFile) {
			return nil, fmt.Errorf("invalid metadata: %s: %w", path, errNotRegularFile)
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

func writeReleaseMetadata(release, metadataReleasePath string, config config, sha string) error {
	metadata := releaseMetadata{
		"schema_version": schemaVersion,
		"tool":           config.name,
		"entrypoint":     config.entrypoint,
		"git_sha":        sha,
		"source_path":    config.sourcePath,
		"branch":         config.branch,
		"remote":         config.remote,
		"installed_at":   time.Now().UTC().Format(time.RFC3339Nano),
		"release_path":   metadataReleasePath,
		"config":         config.raw,
	}
	manifest, slots, err := buildIntegrityManifest(release, metadata, config)
	if err != nil {
		return err
	}
	metadataPath := filepath.Join(release, metadataDir, metadataName)
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0o755); err != nil {
		return err
	}
	manifestPath := filepath.Join(release, metadataDir, integrityName)
	if err := atomicWriteJSON(manifestPath, manifest); err != nil {
		return err
	}
	manifestDigest, err := hashFile(manifestPath)
	if err != nil {
		return err
	}
	metadata["integrity"] = integrityReference{
		Algorithm:      integrityAlgorithm,
		ManifestSHA256: manifestDigest,
	}
	if err := atomicWriteJSON(metadataPath, metadata); err != nil {
		return err
	}
	saveIntegrityCache(release, manifestDigest, nil, slots)
	return nil
}

func buildIntegrityManifest(release string, metadata releaseMetadata, config config) (integrityManifest, []integrityCacheSlot, error) {
	metadataDigest, err := hashMetadataPayload(metadata)
	if err != nil {
		return integrityManifest{}, nil, err
	}
	entries, slots, err := collectIntegrityEntries(release, config, nil, nil)
	if err != nil {
		return integrityManifest{}, nil, err
	}
	return integrityManifest{
		Version:        1,
		MetadataSHA256: metadataDigest,
		Entries:        entries,
	}, slots, nil
}

func hashMetadataPayload(metadata releaseMetadata) (string, error) {
	payload := make(releaseMetadata, len(metadata))
	for key, value := range metadata {
		if key != "integrity" {
			payload[key] = value
		}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return hashBytes(data), nil
}

func integrityPathExcluded(rel string, config config) bool {
	rel = filepath.ToSlash(rel)
	if rel == metadataDir || strings.HasPrefix(rel, metadataDir+"/") ||
		rel == ".cache" || strings.HasPrefix(rel, ".cache/") {
		return true
	}
	for _, injected := range config.inject {
		injected = filepath.ToSlash(injected)
		if rel == injected || strings.HasPrefix(rel, injected+"/") {
			return true
		}
	}
	const pycache = "__pycache__"
	return rel == pycache || strings.HasPrefix(rel, pycache+"/") ||
		strings.HasSuffix(rel, "/"+pycache) || strings.Contains(rel, "/"+pycache+"/")
}

var errNotRegularFile = errors.New("not a regular file")

// openRegularFile opens path for reading only if it is a regular file.
// O_NONBLOCK keeps a FIFO or device at path from blocking the open, and the
// type is checked on the opened descriptor so it cannot be swapped afterwards.
func openRegularFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = &os.PathError{Op: "open", Path: path, Err: errNotRegularFile}
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func hashFile(filePath string) (string, error) {
	file, err := openRegularFile(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func hashBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func atomicWriteJSON(path string, payload any) error {
	return atomicWriteFile(path, func(file io.Writer) error {
		encoder := json.NewEncoder(file)
		encoder.SetIndent("", "  ")
		return encoder.Encode(payload)
	})
}

func atomicWriteFile(path string, write func(io.Writer) error) error {
	tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.%d.tmp", filepath.Base(path), os.Getpid()))
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := write(file); err != nil {
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
	lock, err := acquireToolLock(ctx)
	if err != nil {
		return updateReport{}, err
	}
	defer lock.release()

	sha, err := sourceSHAForUpdate(*config)
	if err != nil {
		return updateReport{}, err
	}
	selectedConfig, err := loadConfigAtRevision(config.sourcePath, sha)
	if err != nil {
		return updateReport{}, err
	}
	if selectedConfig.name != ctx.name {
		return updateReport{}, fmt.Errorf("source config tool %q does not match installed tool %q", selectedConfig.name, ctx.name)
	}
	// Close the race where HEAD changes after source config resolution but before
	// the selected SHA is reloaded.
	if !sameConfig(*config, *selectedConfig) {
		return updateReport{}, fmt.Errorf("source %s changed while selecting release %s; retry the update", configName, sha)
	}
	if err := runConfiguredCommands(selectedConfig.preflight, selectedConfig.sourcePath, nil); err != nil {
		return updateReport{}, err
	}
	release, err := ensureRelease(ctx, *selectedConfig, sha)
	if err != nil {
		return updateReport{}, err
	}

	if err := runSteps(
		// ensureRelease just built or fully rechecked this release.
		releaseStep{"verify release", func() error { return verifyRelease(ctx, release, false, cachedIntegrityCheck) }},
	); err != nil {
		return updateReport{}, err
	}
	if err := activateAndVerifyRelease(ctx, sha, "activate release"); err != nil {
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
	}, nil
}

func rollbackRelease(ctx pinContext) (rollbackReport, error) {
	lock, err := acquireToolLock(ctx)
	if err != nil {
		return rollbackReport{}, err
	}
	defer lock.release()

	previousTarget, err := previousRelease(ctx)
	if err != nil {
		return rollbackReport{}, err
	}
	previousSHA := filepath.Base(previousTarget)

	if err := runSteps(
		releaseStep{"verify previous release", func() error { return verifyRelease(ctx, previousTarget, false, fullIntegrityCheck) }},
	); err != nil {
		return rollbackReport{}, err
	}
	if err := activateAndVerifyRelease(ctx, previousSHA, "activate previous release"); err != nil {
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
	if err != nil {
		return release, err
	}
	if !exists {
		return buildRelease(ctx, config, sha)
	}

	metadataPath := filepath.Join(release, metadataDir, metadataName)
	_, metadataErr := os.Stat(metadataPath)
	if errors.Is(metadataErr, os.ErrNotExist) {
		active, activeErr := releaseIsActive(ctx, release)
		if activeErr != nil {
			return "", activeErr
		}
		if active {
			return "", fmt.Errorf("release %s is active but missing metadata; it was left unchanged; commit a new source revision and retry", sha)
		}
		if err := os.RemoveAll(release); err != nil {
			return release, fmt.Errorf("remove incomplete release %s: %w", release, err)
		}
		return buildRelease(ctx, config, sha)
	}
	if metadataErr != nil {
		return release, metadataErr
	}

	metadata, err := readReleaseMetadata(release)
	if err != nil {
		return "", fmt.Errorf("existing release %s cannot be safely reused: %w", release, err)
	}
	storedConfig, err := loadConfigFromMetadata(metadata)
	if err != nil {
		return "", fmt.Errorf("existing release %s cannot be safely reused: %w", release, err)
	}
	if err := validateMetadata(ctx, release, metadata, *storedConfig); err != nil {
		return "", fmt.Errorf("existing release %s cannot be safely reused: %w", release, err)
	}
	if sameConfig(config, *storedConfig) {
		if err := verifyReleaseIntegrity(release, *storedConfig, fullIntegrityCheck); err != nil {
			return "", fmt.Errorf("existing release %s cannot be safely reused: %w", release, err)
		}
		return release, nil
	}

	active, err := releaseIsActive(ctx, release)
	if err != nil {
		return "", err
	}
	if active {
		return "", fmt.Errorf("release %s is active and was built with different config than committed %s; it was left unchanged; commit a new source revision and retry", sha, configName)
	}
	return "", fmt.Errorf("inactive release %s was built with different config than committed %s; it was left unchanged; move or remove %s and retry", sha, configName, release)
}

func releaseIsActive(ctx pinContext, release string) (bool, error) {
	active, ok, err := releaseSymlink(ctx, ctx.currentLink())
	if err != nil || !ok {
		return false, err
	}
	return filepath.Clean(active) == filepath.Clean(release), nil
}

func sameConfig(left, right config) bool {
	if left.name != right.name || left.entrypoint != right.entrypoint || left.branch != right.branch || left.remote != right.remote {
		return false
	}
	leftRaw, leftErr := json.Marshal(left.raw)
	rightRaw, rightErr := json.Marshal(right.raw)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftRaw, rightRaw)
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
	temp, err := os.MkdirTemp(ctx.releasesDir(), "."+sha+".tmp-")
	if err != nil {
		return "", err
	}

	if err := cleanupOnError(temp, func() error {
		return runSteps(
			releaseStep{"extract source", func() error { return extractGitArchive(config.sourcePath, sha, temp) }},
			releaseStep{"validate archived symlinks", func() error { return validateArchivedSymlinks(temp) }},
			releaseStep{"check runtime paths", func() error { return ensureRuntimePathsAvailable(temp) }},
			releaseStep{"inject runtime paths", func() error { return injectRuntimePaths(ctx, temp, config) }},
		)
	}); err != nil {
		return "", err
	}
	if exists, err := directoryExists(final); err != nil || exists {
		_ = os.RemoveAll(temp)
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("release already exists: %s", final)
	}
	if err := os.Rename(temp, final); err != nil {
		_ = os.RemoveAll(temp)
		return "", err
	}
	if err := cleanupOnError(final, func() error {
		return runSteps(
			releaseStep{"create virtualenv", func() error { return createVenv(final) }},
			releaseStep{"install python runtime", func() error { return installPythonRuntime(final, final, config) }},
			releaseStep{"write metadata", func() error { return writeReleaseMetadata(final, final, config, sha) }},
		)
	}); err != nil {
		return "", err
	}
	return final, nil
}

func cleanupOnError(path string, run func() error) error {
	if err := run(); err != nil {
		return cleanupFailedRelease(path, err, os.RemoveAll)
	}
	return nil
}

func cleanupFailedRelease(path string, cause error, removeAll func(string) error) error {
	if err := removeAll(path); err != nil {
		return errors.Join(cause, fmt.Errorf("cleanup incomplete release: %w\nincomplete path: %s", err, path))
	}
	return cause
}

// extractGitArchive streams a Git revision into tar with bounded time and diagnostics.
func extractGitArchive(repo, sha, destination string) error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultCommandLimits.timeout)
	defer cancel()

	archive := exec.CommandContext(ctx, "git", "archive", "--format=tar", sha)
	configureProcessTree(archive)
	archive.WaitDelay = releaseCommandWaitDelay
	archive.Dir = repo
	pipeReader, pipeWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	defer pipeReader.Close()
	defer pipeWriter.Close()
	archive.Stdout = pipeWriter
	archiveStderr := newBoundedBuffer(defaultCommandLimits.outputLimit)
	archive.Stderr = archiveStderr

	extract := exec.CommandContext(ctx, "tar", "-xf", "-", "-C", destination)
	configureProcessTree(extract)
	extract.WaitDelay = releaseCommandWaitDelay
	extract.Stdin = pipeReader
	extractStderr := newBoundedBuffer(defaultCommandLimits.outputLimit)
	extract.Stderr = extractStderr

	if err := archive.Start(); err != nil {
		return err
	}
	if err := extract.Start(); err != nil {
		_ = pipeReader.Close()
		_ = pipeWriter.Close()
		cancel()
		_ = archive.Wait()
		return err
	}
	_ = pipeReader.Close()
	_ = pipeWriter.Close()

	type pipelineResult struct {
		stage    string
		err      error
		timedOut bool
	}
	results := make(chan pipelineResult, 2)
	wait := func(stage string, command *exec.Cmd) {
		err := command.Wait()
		results <- pipelineResult{
			stage:    stage,
			err:      err,
			timedOut: errors.Is(ctx.Err(), context.DeadlineExceeded),
		}
	}
	go wait("git", archive)
	go wait("tar", extract)

	var archiveResult, extractResult pipelineResult
	failed := false
	timedOut := false
	for range 2 {
		result := <-results
		if result.stage == "git" {
			archiveResult = result
		} else {
			extractResult = result
		}
		timedOut = timedOut || result.timedOut
		if result.err != nil && !result.timedOut && !failed {
			failed = true
			cancel()
		}
	}
	if failed {
		var failures []error
		if archiveResult.err != nil {
			failures = append(failures, fmt.Errorf("git archive failed: %w%s", archiveResult.err, outputDetails(archiveStderr.String())))
		}
		if extractResult.err != nil {
			failures = append(failures, fmt.Errorf("tar extract failed: %w%s", extractResult.err, outputDetails(extractStderr.String())))
		}
		return errors.Join(failures...)
	}
	if timedOut || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		details := pipelineDetails(archiveStderr.String(), extractStderr.String())
		return fmt.Errorf("git archive and tar extract timed out after %s%s", defaultCommandLimits.timeout, details)
	}
	return nil
}

// pipelineDetails formats non-empty diagnostics from the archive pipeline stages.
func pipelineDetails(archiveStderr, extractStderr string) string {
	var details []string
	if text := strings.TrimSpace(archiveStderr); text != "" {
		details = append(details, "git: "+text)
	}
	if text := strings.TrimSpace(extractStderr); text != "" {
		details = append(details, "tar: "+text)
	}
	if len(details) == 0 {
		return ""
	}
	return ": " + strings.Join(details, "; ")
}

// validateArchivedSymlinks ensures every archived symlink resolves inside the release.
func validateArchivedSymlinks(release string) error {
	root, err := os.OpenRoot(release)
	if err != nil {
		return err
	}
	defer root.Close()

	return filepath.WalkDir(release, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink == 0 {
			return nil
		}
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(release, path)
		if err != nil {
			return err
		}
		if _, err := root.Stat(rel); err != nil {
			return fmt.Errorf("archived symlink must resolve inside release: %s -> %s: %w", path, target, err)
		}
		return nil
	})
}

func ensureRuntimePathsAvailable(release string) error {
	for _, rel := range []string{venvDir, metadataDir, ".cache"} {
		path := filepath.Join(release, rel)
		_, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("reserved runtime path already exists in archived checkout: %s", path)
	}
	return nil
}

func injectRuntimePaths(ctx pinContext, releaseSource string, config config) error {
	if len(config.inject) != 0 {
		if err := prepareInjectedSharedRoot(ctx.sharedDir()); err != nil {
			return err
		}
	}
	for _, path := range config.inject {
		backing := ctx.sharedPath(path)
		if err := ensureInjectTargetAvailable(releaseSource, path); err != nil {
			return err
		}
		if err := ensureInjectedBackingPath(backing, filepath.Join(config.sourcePath, path)); err != nil {
			return err
		}
		target := filepath.Join(releaseSource, path)
		linkTarget, err := relativeSymlinkTarget(target, backing)
		if err != nil {
			return err
		}
		if err := os.Symlink(linkTarget, target); err != nil {
			return err
		}
	}
	return nil
}

func ensureInjectTargetAvailable(releaseSource, rel string) error {
	if err := ensureInjectParentDirs(releaseSource, filepath.Dir(rel)); err != nil {
		return err
	}
	target := filepath.Join(releaseSource, rel)
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("inject target already exists in archived checkout: %s", target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func ensureInjectParentDirs(releaseSource, relParent string) error {
	if relParent == "." {
		return nil
	}
	current := releaseSource
	for _, part := range strings.Split(relParent, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return os.MkdirAll(filepath.Join(releaseSource, relParent), 0o755)
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("inject parent is a symlink in archived checkout: %s", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("inject parent is not a directory in archived checkout: %s", current)
		}
	}
	return nil
}

func ensureInjectedBackingPath(backing, source string) error {
	if _, err := os.Lstat(backing); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	sourceInfo, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("injected path is missing; create %s or seed it at %s", backing, source)
	}
	if err != nil {
		return err
	}
	if sourceInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("injected source path is a symlink: %s", source)
	}

	if err := os.MkdirAll(filepath.Dir(backing), 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(backing), fmt.Sprintf(".%s.%d.tmp", filepath.Base(backing), os.Getpid()))
	_ = os.RemoveAll(tmp)
	if sourceInfo.IsDir() {
		if err := copyDirectory(tmp, source); err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
	} else {
		if err := copyRegularFile(tmp, source, privateInjectedMode(sourceInfo.Mode())); err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, backing); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	return nil
}

func copyDirectory(destination, source string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("injected source path contains symlink: %s", path)
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, privateInjectedMode(info.Mode()))
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("injected source path contains unsupported file: %s", path)
		}
		return copyRegularFile(target, path, privateInjectedMode(info.Mode()))
	})
}

func privateInjectedMode(mode os.FileMode) os.FileMode {
	return mode.Perm() & 0o700
}

func copyRegularFile(destination, source string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()

	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func relativeSymlinkTarget(link, target string) (string, error) {
	linkDir, err := filepath.Abs(filepath.Dir(link))
	if err != nil {
		return "", err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	return filepath.Rel(linkDir, targetAbs)
}

func createVenv(release string) error {
	venvPath := filepath.Join(release, venvDir)
	env := pythonInstallEnv(release)
	if uv, err := exec.LookPath("uv"); err == nil {
		_, err := runCommand([]string{uv, "venv", venvPath}, release, env)
		return err
	}

	python, err := pythonCommand()
	if err != nil {
		return err
	}
	_, err = runCommand([]string{python, "-m", "venv", venvPath}, release, env)
	return err
}

func installPythonRuntime(release, sourceRoot string, config config) error {
	switch config.source.kind {
	case sourcePackage:
		return installPythonPackage(release, filepath.Join(sourceRoot, config.source.path))
	case sourceScript:
		return installPythonScript(release, sourceRoot, config)
	default:
		return fmt.Errorf("unknown source kind %q", config.source.kind)
	}
}

func installPythonPackage(release, source string) error {
	if err := requireFile(filepath.Join(source, "pyproject.toml"), "missing pyproject.toml"); err != nil {
		return err
	}

	python := filepath.Join(release, venvDir, "bin", "python")
	env := pythonInstallEnv(release)
	if uv, err := exec.LookPath("uv"); err == nil {
		_, err := runCommand([]string{uv, "pip", "install", "--python", python, "."}, source, env)
		return err
	}

	_, err := runCommand([]string{python, "-m", "pip", "install", "."}, source, env)
	return err
}

func installPythonScript(release, sourceRoot string, config config) error {
	script := filepath.Join(sourceRoot, config.source.path)
	if err := requireFile(script, "missing script"); err != nil {
		return err
	}
	if config.requirements != "" {
		if err := installPythonRequirements(release, sourceRoot, config.requirements); err != nil {
			return err
		}
	}
	return writePythonScriptEntrypoint(release, config.entrypoint, script)
}

func installPythonRequirements(release, source, requirements string) error {
	requirementsPath := filepath.Join(source, requirements)
	if err := requireFile(requirementsPath, "missing requirements"); err != nil {
		return err
	}

	python := filepath.Join(release, venvDir, "bin", "python")
	env := pythonInstallEnv(release)
	if uv, err := exec.LookPath("uv"); err == nil {
		_, err := runCommand([]string{uv, "pip", "install", "--python", python, "-r", requirementsPath}, source, env)
		return err
	}

	_, err := runCommand([]string{python, "-m", "pip", "install", "-r", requirementsPath}, source, env)
	return err
}

func pythonInstallEnv(release string) []string {
	env := os.Environ()
	env = appendDefaultEnv(env, "UV_CACHE_DIR", filepath.Join(release, ".cache", "uv"))
	env = appendDefaultEnv(env, "PIP_CACHE_DIR", filepath.Join(release, ".cache", "pip"))
	return env
}

func appendDefaultEnv(env []string, key, value string) []string {
	prefix := key + "="
	updated := make([]string, 0, len(env)+1)
	found := false
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			if strings.TrimPrefix(item, prefix) == "" {
				continue
			}
			found = true
		}
		updated = append(updated, item)
	}
	if found {
		return updated
	}
	return append(updated, prefix+value)
}

func writePythonScriptEntrypoint(release, entrypoint, script string) error {
	path := filepath.Join(release, venvDir, "bin", entrypoint)
	if pathExists(path) {
		return fmt.Errorf("entrypoint %q conflicts with an existing file in the release virtualenv", entrypoint)
	}
	python := filepath.Join(release, venvDir, "bin", "python")
	wrapper := "#!/bin/sh\ncd " + shellQuote(release) + " || exit 1\nexec " + shellQuote(python) + " " + shellQuote(script) + " \"$@\"\n"
	return os.WriteFile(path, []byte(wrapper), 0o755)
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func pythonCommand() (string, error) {
	for _, name := range []string{"python3", "python"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("python3 or python is required to build releases")
}

func verifyActive(ctx pinContext, precheck integrityCheck) (releaseMetadata, error) {
	release, err := activeRelease(ctx)
	if err != nil {
		return nil, err
	}
	if err := verifyRelease(ctx, release, true, precheck); err != nil {
		return nil, err
	}
	return readReleaseMetadata(release)
}

// verifyRelease checks integrity before and after the configured verify
// commands. precheck may be cachedIntegrityCheck only when the caller fully
// checked this release earlier in the same command and no release code has run
// since; the recheck after verify commands is always full.
func verifyRelease(ctx pinContext, release string, expectActive bool, precheck integrityCheck) error {
	if err := requireDirectory(release, "release"); err != nil {
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
	entrypoint := filepath.Join(release, venvDir, "bin", metadata.string("entrypoint"))

	return runSteps(
		releaseStep{"validate metadata", func() error { return validateMetadata(ctx, release, metadata, *config) }},
		releaseStep{"check injected paths", func() error { return verifyInjectedPaths(ctx, release, metadata, *config) }},
		releaseStep{"check entrypoint", func() error { return requireFile(entrypoint, "missing entrypoint") }},
		releaseStep{"check release integrity", func() error { return verifyReleaseIntegrity(release, *config, precheck) }},
		releaseStep{"check active link", func() error {
			if !expectActive {
				return nil
			}
			return requireActiveTarget(ctx, release)
		}},
		releaseStep{"run verify commands", func() error {
			return runConfiguredCommands(config.verify, release, entrypointEnv(entrypoint))
		}},
		releaseStep{"recheck release integrity", func() error { return verifyReleaseIntegrity(release, *config, fullIntegrityCheck) }},
	)
}

func verifyReleaseIntegrity(release string, config config, check integrityCheck) error {
	metadata, err := readReleaseMetadata(release)
	if err != nil {
		return err
	}
	reference, err := parseIntegrityReference(metadata)
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(release, metadataDir, integrityName)
	manifestFile, err := openRegularFile(manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("release integrity manifest is missing: %s", manifestPath)
	}
	if errors.Is(err, errNotRegularFile) {
		return fmt.Errorf("invalid release integrity manifest: %s: %w", manifestPath, errNotRegularFile)
	}
	if err != nil {
		return err
	}
	manifestData, err := io.ReadAll(manifestFile)
	manifestFile.Close()
	if err != nil {
		return err
	}
	if actual := hashBytes(manifestData); actual != reference.ManifestSHA256 {
		return fmt.Errorf("release integrity manifest digest changed: %s", manifestPath)
	}

	var manifest integrityManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("invalid release integrity manifest: %s: %w", manifestPath, err)
	}
	if manifest.Version != 1 {
		return fmt.Errorf("unsupported release integrity manifest version: %d", manifest.Version)
	}
	metadataDigest, err := hashMetadataPayload(metadata)
	if err != nil {
		return err
	}
	if metadataDigest != manifest.MetadataSHA256 {
		return fmt.Errorf("release metadata digest changed: %s", filepath.Join(release, metadataDir, metadataName))
	}
	cached := loadIntegrityCache(release, reference.ManifestSHA256, len(manifest.Entries))
	var reuse []integrityCacheSlot
	if check == cachedIntegrityCheck {
		reuse = cached
	}
	actual, slots, err := collectIntegrityEntries(release, config, manifest.Entries, reuse)
	if err != nil {
		return err
	}
	if err := compareIntegrityEntries(manifest.Entries, actual); err != nil {
		return err
	}
	// actual equals the manifest, so slots stay aligned with its entries.
	saveIntegrityCache(release, reference.ManifestSHA256, cached, slots)
	return nil
}

func parseIntegrityReference(metadata releaseMetadata) (integrityReference, error) {
	raw, ok := metadata["integrity"]
	if !ok {
		return integrityReference{}, fmt.Errorf("release has no integrity manifest; commit a new clean source revision and run pin update")
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return integrityReference{}, err
	}
	var reference integrityReference
	if err := json.Unmarshal(data, &reference); err != nil {
		return integrityReference{}, fmt.Errorf("invalid release integrity metadata: %w", err)
	}
	if reference.Algorithm != integrityAlgorithm || len(reference.ManifestSHA256) != sha256.Size*2 {
		return integrityReference{}, fmt.Errorf("invalid release integrity metadata")
	}
	return reference, nil
}

func compareIntegrityEntries(expected, actual []integrityEntry) error {
	// Scanned entries are valid and unique by construction, so an identical
	// manifest needs no further validation. The map-based comparison below
	// explains any difference.
	if slices.Equal(expected, actual) {
		return nil
	}
	expectedByPath := make(map[string]integrityEntry, len(expected))
	for _, entry := range expected {
		if !validIntegrityEntry(entry) {
			return fmt.Errorf("invalid release integrity entry: %q", entry.Path)
		}
		if _, exists := expectedByPath[entry.Path]; exists {
			return fmt.Errorf("duplicate release integrity entry: %s", entry.Path)
		}
		expectedByPath[entry.Path] = entry
	}
	actualByPath := make(map[string]integrityEntry, len(actual))
	for _, entry := range actual {
		actualByPath[entry.Path] = entry
	}
	for _, wanted := range expected {
		got, exists := actualByPath[wanted.Path]
		if !exists {
			return fmt.Errorf("release integrity mismatch: missing %s", wanted.Path)
		}
		if got.Type != wanted.Type {
			return fmt.Errorf("release integrity mismatch: type changed for %s", wanted.Path)
		}
		if got.Mode != wanted.Mode {
			return fmt.Errorf("release integrity mismatch: mode changed for %s", wanted.Path)
		}
		if got.SHA256 != wanted.SHA256 {
			return fmt.Errorf("release integrity mismatch: content changed for %s", wanted.Path)
		}
		delete(actualByPath, wanted.Path)
	}
	for _, entry := range actual {
		if _, exists := actualByPath[entry.Path]; exists {
			return fmt.Errorf("release integrity mismatch: unexpected %s", entry.Path)
		}
	}
	return nil
}

func validIntegrityEntry(entry integrityEntry) bool {
	if entry.Path == "" || entry.Path != path.Clean(entry.Path) || path.IsAbs(entry.Path) || entry.Path == ".." || strings.HasPrefix(entry.Path, "../") {
		return false
	}
	if entry.Type != "file" && entry.Type != "directory" && entry.Type != "symlink" {
		return false
	}
	if len(entry.Mode) != 4 {
		return false
	}
	if entry.Type == "directory" {
		return entry.SHA256 == ""
	}
	return len(entry.SHA256) == sha256.Size*2
}

func verifyInjectedPaths(ctx pinContext, release string, metadata releaseMetadata, config config) error {
	if metadata.schemaVersion() == 2 {
		return verifyLegacyInjectedFiles(release, config)
	}
	return verifyInjectedRuntimePaths(ctx, release, config)
}

func verifyInjectedRuntimePaths(ctx pinContext, release string, config config) error {
	for _, path := range config.inject {
		backing := ctx.sharedPath(path)
		if _, err := os.Stat(backing); errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("missing injected path: %s", backing)
		} else if err != nil {
			return err
		}
		target := filepath.Join(release, path)
		linkTarget, err := relativeSymlinkTarget(target, backing)
		if err != nil {
			return err
		}
		if err := verifyReleaseSymlink(target, linkTarget, "injected path"); err != nil {
			return err
		}
	}
	return nil
}

func verifyLegacyInjectedFiles(release string, config config) error {
	for _, path := range config.inject {
		source := filepath.Join(config.sourcePath, path)
		if err := requireFile(source, "missing injected file"); err != nil {
			return err
		}
		if err := verifyReleaseSymlink(filepath.Join(release, path), source, "injected file"); err != nil {
			return err
		}
	}
	return nil
}

func verifyReleaseSymlink(target, expected, label string) error {
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("missing %s symlink: %s", label, target)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("%s target is not a symlink: %s", label, target)
	}
	if readlink(target) != expected {
		return fmt.Errorf("%s symlink points to %s, expected %s", label, readlink(target), expected)
	}
	return nil
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
	version, ok := metadata.int("schema_version")
	if !ok || version != 2 && version != schemaVersion {
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

type symlinkSnapshot struct {
	path   string
	target string
	exists bool
}

type releaseLinkSnapshot struct {
	current  symlinkSnapshot
	previous symlinkSnapshot
}

func activateAndVerifyRelease(ctx pinContext, sha, activationStep string) error {
	links, err := captureReleaseLinks(ctx)
	if err != nil {
		return fmt.Errorf("capture release links: %w", err)
	}
	var restoreOnce sync.Once
	var restoreErr error
	restore := func() error {
		restoreOnce.Do(func() {
			restoreErr = links.restore()
		})
		return restoreErr
	}

	err = runWithSignalRestoration(restore, func() error {
		return runSteps(
			releaseStep{activationStep, func() error { return activateRelease(ctx, sha) }},
			releaseStep{"verify active release", func() error {
				// Activation follows a full recheck of this release.
				_, err := verifyActive(ctx, cachedIntegrityCheck)
				return err
			}},
		)
	})
	if err == nil {
		return nil
	}
	if restoreErr := restore(); restoreErr != nil {
		return errors.Join(err, fmt.Errorf("restore release links: %w", restoreErr))
	}
	return err
}

func runWithSignalRestoration(restore func() error, run func() error) error {
	signals := make(chan os.Signal, 1)
	done := make(chan struct{})
	handlerDone := make(chan struct{})
	signal.Notify(signals, activationSignals()...)

	handleSignal := func(received os.Signal) {
		_ = restore()
		os.Exit(exitCodeForSignal(received))
	}
	go func() {
		defer close(handlerDone)
		select {
		case received := <-signals:
			handleSignal(received)
		case <-done:
			// Prefer a signal already queued before signal.Stop completed over
			// normal shutdown of the handler.
			select {
			case received := <-signals:
				handleSignal(received)
			default:
			}
		}
	}()

	err := run()
	signal.Stop(signals)
	close(done)
	<-handlerDone
	return err
}

func captureReleaseLinks(ctx pinContext) (releaseLinkSnapshot, error) {
	current, err := captureSymlink(ctx.currentLink())
	if err != nil {
		return releaseLinkSnapshot{}, err
	}
	previous, err := captureSymlink(ctx.previousLink())
	if err != nil {
		return releaseLinkSnapshot{}, err
	}
	return releaseLinkSnapshot{current: current, previous: previous}, nil
}

func captureSymlink(path string) (symlinkSnapshot, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return symlinkSnapshot{path: path}, nil
	}
	if err != nil {
		return symlinkSnapshot{}, err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return symlinkSnapshot{}, fmt.Errorf("%s exists but is not a symlink", path)
	}
	target, err := os.Readlink(path)
	if err != nil {
		return symlinkSnapshot{}, err
	}
	return symlinkSnapshot{path: path, target: target, exists: true}, nil
}

func (snapshot releaseLinkSnapshot) restore() error {
	return errors.Join(
		restoreSymlink(snapshot.previous),
		restoreSymlink(snapshot.current),
	)
}

func restoreSymlink(snapshot symlinkSnapshot) error {
	if snapshot.exists {
		return replaceSymlink(snapshot.path, snapshot.target)
	}
	info, err := os.Lstat(snapshot.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("cannot restore missing symlink because %s is not a symlink", snapshot.path)
	}
	return os.Remove(snapshot.path)
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
	metadata, err := readReleaseMetadata(target)
	if err != nil {
		return err
	}
	config, err := loadConfigFromMetadata(metadata)
	if err != nil {
		return err
	}
	if err := validateMetadata(ctx, target, metadata, *config); err != nil {
		return err
	}
	if err := verifyInjectedPaths(ctx, target, metadata, *config); err != nil {
		return err
	}
	// Every caller fully rechecks the release immediately before activation.
	if err := verifyReleaseIntegrity(target, *config, cachedIntegrityCheck); err != nil {
		return err
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

// compareCommits reports the ancestry relationship between active and target commits.
func compareCommits(repo, active, target string) (string, error) {
	if active == target {
		return checkCurrent, nil
	}
	activeIsAncestor, err := gitOK(repo, "merge-base", "--is-ancestor", active, target)
	if err != nil {
		return "", err
	}
	targetIsAncestor, err := gitOK(repo, "merge-base", "--is-ancestor", target, active)
	if err != nil {
		return "", err
	}
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

// gitOK reports whether a Git predicate command succeeded and preserves execution errors.
func gitOK(cwd string, args ...string) (bool, error) {
	result, err := runGit(cwd, args...)
	if err == nil {
		return true, nil
	}
	if result.exitCode == 1 {
		return false, nil
	}
	return false, err
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

// runCommand executes a release subprocess with the default resource limits.
func runCommand(args []string, cwd string, env []string) (commandResult, error) {
	return runCommandWithLimits(args, cwd, env, defaultCommandLimits)
}

// runCommandWithLimits executes a subprocess with bounded runtime and captured output.
func runCommandWithLimits(args []string, cwd string, env []string, limits commandLimits) (commandResult, error) {
	if len(args) == 0 {
		return commandResult{}, fmt.Errorf("empty command")
	}
	commandArgs := append([]string(nil), args...)
	if env != nil && !strings.ContainsRune(commandArgs[0], os.PathSeparator) {
		if resolved, ok := lookPathInEnv(commandArgs[0], env); ok {
			commandArgs[0] = resolved
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), limits.timeout)
	defer cancel()
	command := exec.CommandContext(ctx, commandArgs[0], commandArgs[1:]...)
	configureProcessTree(command)
	command.WaitDelay = releaseCommandWaitDelay
	command.Dir = cwd
	if env != nil {
		command.Env = env
	}
	stdout := newBoundedBuffer(limits.outputLimit)
	stderr := newBoundedBuffer(limits.outputLimit)
	command.Stdout = stdout
	command.Stderr = stderr

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
		details := strings.TrimSpace(result.stderr)
		if details == "" {
			details = strings.TrimSpace(result.stdout)
		}
		suffix := outputDetails(details)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return result, fmt.Errorf("command timed out after %s in %s: %s%s", limits.timeout, cwd, quoteCommand(args), suffix)
		}
		return result, fmt.Errorf("command failed in %s: %s%s", cwd, quoteCommand(args), suffix)
	}
	return result, nil
}

// outputDetails formats non-empty subprocess output for inclusion in an error.
func outputDetails(output string) string {
	details := strings.TrimSpace(output)
	if details == "" {
		return ""
	}
	return ": " + details
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
