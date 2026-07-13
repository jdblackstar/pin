package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/BurntSushi/toml"
)

const configName = "pin.toml"

type config struct {
	name         string
	entrypoint   string
	source       installSource
	requirements string
	inject       []string
	branch       string
	remote       string
	preflight    [][]string
	verify       [][]string
	sourcePath   string
	configPath   string
	raw          map[string]any
}

type sourceKind string

const (
	sourcePackage sourceKind = "package"
	sourceScript  sourceKind = "script"
)

type installSource struct {
	path string
	kind sourceKind
}

type pinContext struct {
	name    string
	pinHome string
	config  *config
}

func (ctx pinContext) toolRoot() string {
	return filepath.Join(ctx.pinHome, ctx.name)
}

func (ctx pinContext) releasesDir() string {
	return filepath.Join(ctx.toolRoot(), "releases")
}

func (ctx pinContext) sharedDir() string {
	return filepath.Join(ctx.toolRoot(), "shared")
}

func (ctx pinContext) sharedPath(rel string) string {
	return filepath.Join(ctx.sharedDir(), rel)
}

func (ctx pinContext) currentLink() string {
	return filepath.Join(ctx.toolRoot(), "current")
}

func (ctx pinContext) previousLink() string {
	return filepath.Join(ctx.toolRoot(), "previous")
}

func resolveContext(toolOrPath string, hasArg bool, opts globalOptions) (pinContext, error) {
	var candidate string
	if !hasArg {
		wd, err := os.Getwd()
		if err != nil {
			return pinContext{}, err
		}
		candidate = wd
	} else {
		candidate = expandPath(toolOrPath)
	}

	if !hasArg || pathExists(candidate) {
		configPath, ok := findConfig(candidate)
		if !ok {
			if !hasArg {
				wd, _ := os.Getwd()
				return pinContext{}, fmt.Errorf("no %s found from %s", configName, wd)
			}
			return pinContext{}, fmt.Errorf("no %s found at %s", configName, candidate)
		}
		config, err := loadConfig(configPath)
		if err != nil {
			return pinContext{}, err
		}
		ctx := pinContext{config.name, opts.pinHome, config}
		metadata, err := readMetadataForTool(opts.pinHome, ctx.name)
		if err != nil {
			if ok, legacyErr := legacyInstallExists(ctx.name, opts); legacyErr == nil && ok {
				ctx.pinHome = opts.legacyPinHome
				return ctx, nil
			}
			return pinContext{}, err
		}
		if metadata == nil {
			if ok, err := legacyInstallExists(ctx.name, opts); err != nil || ok {
				if err != nil {
					return pinContext{}, err
				}
				ctx.pinHome = opts.legacyPinHome
			}
		}
		return ctx, nil
	}

	if looksLikePath(toolOrPath) {
		return pinContext{}, fmt.Errorf("path does not exist: %s", candidate)
	}

	return resolveToolContext(toolOrPath, opts)
}

func resolveToolContext(tool string, opts globalOptions) (pinContext, error) {
	ctx, metadata, err := resolveToolInstall(tool, opts)
	if err != nil || metadata == nil {
		return ctx, err
	}
	config, err := loadConfigFromMetadata(metadata)
	if err != nil {
		return pinContext{}, err
	}
	ctx.config = config
	return ctx, nil
}

func resolveToolInstall(tool string, opts globalOptions) (pinContext, releaseMetadata, error) {
	name, err := validatePathSegment(tool, "tool name")
	if err != nil {
		return pinContext{}, nil, err
	}
	ctx := pinContext{name: name, pinHome: opts.pinHome}
	metadata, err := readMetadataForTool(opts.pinHome, name)
	if err != nil {
		if legacy, legacyMetadata, ok, legacyErr := legacyInstall(name, opts); legacyErr == nil && ok {
			return legacy, legacyMetadata, nil
		}
		return pinContext{}, nil, err
	}
	if metadata == nil {
		if legacy, legacyMetadata, ok, err := legacyInstall(name, opts); err != nil || ok {
			return legacy, legacyMetadata, err
		}
	}
	if metadata != nil && metadata.string("tool") != name {
		return pinContext{}, nil, fmt.Errorf("metadata tool %q does not match %q", metadata.string("tool"), name)
	}
	return ctx, metadata, nil
}

func legacyContext(name string, opts globalOptions) (pinContext, bool, error) {
	ctx, metadata, ok, err := legacyInstall(name, opts)
	if err != nil || !ok {
		return pinContext{}, ok, err
	}
	config, err := loadConfigFromMetadata(metadata)
	if err != nil {
		return pinContext{}, true, err
	}
	ctx.config = config
	return ctx, true, nil
}

func legacyInstall(name string, opts globalOptions) (pinContext, releaseMetadata, bool, error) {
	if opts.legacyPinHome == "" {
		return pinContext{}, nil, false, nil
	}
	if filepath.Clean(opts.legacyPinHome) == filepath.Clean(opts.pinHome) {
		return pinContext{}, nil, false, nil
	}
	ctx := pinContext{name: name, pinHome: opts.legacyPinHome}
	metadata, ok, err := legacyMetadata(ctx)
	if err != nil || !ok {
		return pinContext{}, nil, ok, err
	}
	return ctx, metadata, true, nil
}

func resolveSourceContext(toolOrPath string, hasArg bool, opts globalOptions) (pinContext, error) {
	var candidate string
	if !hasArg {
		wd, err := os.Getwd()
		if err != nil {
			return pinContext{}, err
		}
		candidate = wd
	} else {
		candidate = expandPath(toolOrPath)
	}

	if !hasArg || pathExists(candidate) {
		configPath, ok := findConfig(candidate)
		if !ok {
			if !hasArg {
				wd, _ := os.Getwd()
				return pinContext{}, fmt.Errorf("no %s found from %s", configName, wd)
			}
			return pinContext{}, fmt.Errorf("no %s found at %s", configName, candidate)
		}
		config, err := loadCommittedConfig(filepath.Dir(configPath))
		if err != nil {
			return pinContext{}, err
		}
		return sourceContextForConfig(config, opts)
	}

	if looksLikePath(toolOrPath) {
		return pinContext{}, fmt.Errorf("path does not exist: %s", candidate)
	}

	ctx, metadata, err := resolveToolInstall(toolOrPath, opts)
	if err != nil {
		return pinContext{}, err
	}
	if metadata == nil {
		return pinContext{}, fmt.Errorf("no installed release for %s; pass a repo path with %s", ctx.name, configName)
	}
	sourcePath := metadata.string("source_path")
	if sourcePath == "" {
		return pinContext{}, fmt.Errorf("installed metadata for %s does not include source_path", ctx.name)
	}
	config, err := loadCommittedConfig(sourcePath)
	if err != nil {
		return pinContext{}, err
	}
	if config.name != ctx.name {
		return pinContext{}, fmt.Errorf("source config tool %q does not match installed tool %q", config.name, ctx.name)
	}
	ctx.config = config
	return ctx, nil
}

func sourceContextForConfig(config *config, opts globalOptions) (pinContext, error) {
	ctx := pinContext{name: config.name, pinHome: opts.pinHome, config: config}
	metadata, err := readMetadataForTool(opts.pinHome, ctx.name)
	if err != nil {
		if ok, legacyErr := legacyInstallExists(ctx.name, opts); legacyErr == nil && ok {
			ctx.pinHome = opts.legacyPinHome
			return ctx, nil
		}
		return pinContext{}, err
	}
	if metadata == nil {
		if ok, err := legacyInstallExists(ctx.name, opts); err != nil || ok {
			if err != nil {
				return pinContext{}, err
			}
			ctx.pinHome = opts.legacyPinHome
		}
	}
	return ctx, nil
}

func legacyInstallExists(name string, opts globalOptions) (bool, error) {
	if opts.legacyPinHome == "" {
		return false, nil
	}
	if filepath.Clean(opts.legacyPinHome) == filepath.Clean(opts.pinHome) {
		return false, nil
	}
	ctx := pinContext{name: name, pinHome: opts.legacyPinHome}
	_, ok, err := legacyMetadata(ctx)
	return ok, err
}

func legacyMetadata(ctx pinContext) (releaseMetadata, bool, error) {
	ok, err := legacyLayoutExists(ctx)
	if err != nil || !ok {
		return nil, ok, err
	}
	metadata, err := readCurrentMetadata(ctx)
	if err != nil {
		return nil, true, err
	}
	if metadata == nil {
		return nil, false, nil
	}
	if metadata.string("tool") != ctx.name {
		return nil, true, fmt.Errorf("legacy metadata tool %q does not match %q", metadata.string("tool"), ctx.name)
	}
	return metadata, true, nil
}

func legacyLayoutExists(ctx pinContext) (bool, error) {
	releases, err := os.Stat(ctx.releasesDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if !releases.IsDir() {
		return false, nil
	}
	current, err := os.Lstat(ctx.currentLink())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return current.Mode()&os.ModeSymlink != 0, nil
}

func findConfig(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	if !info.IsDir() {
		if filepath.Base(path) == configName {
			return path, true
		}
		return "", false
	}
	configPath := filepath.Join(path, configName)
	if pathExists(configPath) {
		return configPath, true
	}
	return "", false
}

func loadConfig(path string) (*config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	return decodeConfig(data, filepath.Dir(abs), abs)
}

func decodeConfig(data []byte, sourcePath, configPath string) (*config, error) {
	raw := map[string]any{}
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", configPath, err)
	}

	name, err := requirePathSegment(raw, "name")
	if err != nil {
		return nil, err
	}
	entrypoint, err := optionalPathSegment(raw, "entrypoint", name)
	if err != nil {
		return nil, err
	}
	return buildConfig(raw, name, entrypoint, "", "", sourcePath, configPath)
}

func loadCommittedConfig(sourcePath string) (*config, error) {
	configPath, gitPath, committed, err := committedConfig(sourcePath, "HEAD")
	if err != nil {
		return nil, err
	}
	workingSHA, err := gitOutput(sourcePath, "hash-object", "--path="+gitPath, configPath)
	if err != nil {
		return nil, err
	}
	committedSHA, err := gitOutput(sourcePath, "rev-parse", "HEAD:"+gitPath)
	if err != nil {
		return nil, err
	}
	if workingSHA != committedSHA {
		return nil, fmt.Errorf("source %s has local modifications; commit or restore it before continuing", configName)
	}
	return decodeConfig(committed, filepath.Dir(configPath), configPath)
}

func loadConfigAtRevision(sourcePath, revision string) (*config, error) {
	configPath, _, committed, err := committedConfig(sourcePath, revision)
	if err != nil {
		return nil, err
	}
	return decodeConfig(committed, filepath.Dir(configPath), configPath)
}

func committedConfig(sourcePath, revision string) (string, string, []byte, error) {
	if err := ensureGitRepo(sourcePath); err != nil {
		return "", "", nil, err
	}
	configPath := filepath.Join(sourcePath, configName)
	info, err := os.Lstat(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", nil, fmt.Errorf("missing source config: %s", configPath)
	}
	if err != nil {
		return "", "", nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", "", nil, fmt.Errorf("source config must not be a symlink: %s", configPath)
	}
	if !info.Mode().IsRegular() {
		return "", "", nil, fmt.Errorf("source config is not a regular file: %s", configPath)
	}
	topLevel, err := gitOutput(sourcePath, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", nil, err
	}
	canonicalTopLevel, err := filepath.EvalSymlinks(topLevel)
	if err != nil {
		return "", "", nil, err
	}
	canonicalConfigPath, err := filepath.EvalSymlinks(configPath)
	if err != nil {
		return "", "", nil, err
	}
	rel, err := filepath.Rel(canonicalTopLevel, canonicalConfigPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", nil, fmt.Errorf("source config is outside the Git checkout: %s", configPath)
	}
	rel = filepath.ToSlash(rel)
	if _, err := runGit(sourcePath, "ls-files", "--error-unmatch", "--", rel); err != nil {
		return "", "", nil, fmt.Errorf("source config is not tracked by Git: %s", configPath)
	}
	result, err := runGit(sourcePath, "show", revision+":"+rel)
	if err != nil {
		return "", "", nil, fmt.Errorf("source config is not committed at %s: %s", revision, configPath)
	}
	// Keep the caller-facing checkout path; canonical paths are only needed for
	// containment and Git object lookup.
	return configPath, rel, []byte(result.stdout), nil
}

func loadConfigFromMetadata(metadata releaseMetadata) (*config, error) {
	sourcePath := metadata.string("source_path")
	raw, ok := metadata["config"].(map[string]any)
	if sourcePath == "" || !ok {
		return nil, fmt.Errorf("active metadata for %s does not include a usable config", metadata.string("tool"))
	}
	raw = migrateLegacyConfig(raw)
	name, err := validatePathSegment(metadata.string("tool"), "metadata key 'tool'")
	if err != nil {
		return nil, err
	}
	entrypoint, err := validatePathSegment(metadata.string("entrypoint"), "metadata key 'entrypoint'")
	if err != nil {
		return nil, err
	}
	return buildConfig(raw, name, entrypoint, metadata.string("branch"), metadata.string("remote"), sourcePath, filepath.Join(sourcePath, configName))
}

func migrateLegacyConfig(raw map[string]any) map[string]any {
	script, hasScript := raw["script"].(string)
	if !hasScript || script == "" {
		return raw
	}
	if source, ok := raw["source"].(string); ok && source != "" {
		return raw
	}
	migrated := make(map[string]any, len(raw))
	for key, value := range raw {
		if key != "script" {
			migrated[key] = value
		}
	}
	migrated["source"] = script
	return migrated
}

func buildConfig(raw map[string]any, name, entrypoint, branch, remote, sourcePath, configPath string) (*config, error) {
	var err error
	if branch == "" {
		branch, err = optionalString(raw, "branch", "main")
		if err != nil {
			return nil, err
		}
	}
	if remote == "" {
		remote, err = optionalString(raw, "remote", "origin")
		if err != nil {
			return nil, err
		}
	}

	if _, ok := raw["script"]; ok {
		return nil, fmt.Errorf("%s key %q has been replaced by key %q", configName, "script", "source")
	}
	source, err := parseInstallSource(raw)
	if err != nil {
		return nil, err
	}

	requirements, err := optionalRelativePath(raw, "requirements", "")
	if err != nil {
		return nil, err
	}
	if requirements != "" && source.kind != sourceScript {
		return nil, fmt.Errorf("%s key %q requires key %q to point to a Python script", configName, "requirements", "source")
	}
	inject, err := optionalRelativePathList(raw, "inject", nil)
	if err != nil {
		return nil, err
	}

	preflight := [][]string{}
	if value, ok := raw["preflight"]; ok {
		preflight, err = parseCommands(value, "preflight")
		if err != nil {
			return nil, err
		}
	}

	verify := [][]string{{entrypoint, "--help"}}
	if value, ok := raw["verify"]; ok {
		verify, err = parseCommands(value, "verify")
		if err != nil {
			return nil, err
		}
	}

	return &config{
		name:         name,
		entrypoint:   entrypoint,
		source:       source,
		requirements: requirements,
		inject:       inject,
		branch:       branch,
		remote:       remote,
		preflight:    preflight,
		verify:       verify,
		sourcePath:   sourcePath,
		configPath:   configPath,
		raw:          raw,
	}, nil
}

func requireString(raw map[string]any, key string) (string, error) {
	value, ok := raw[key].(string)
	if !ok || value == "" {
		return "", fmt.Errorf("%s requires non-empty string key %q", configName, key)
	}
	return value, nil
}

func requirePathSegment(raw map[string]any, key string) (string, error) {
	value, err := requireString(raw, key)
	if err != nil {
		return "", err
	}
	return validatePathSegment(value, fmt.Sprintf("%s key %q", configName, key))
}

func optionalPathSegment(raw map[string]any, key, fallback string) (string, error) {
	value, err := optionalString(raw, key, fallback)
	if err != nil {
		return "", err
	}
	return validatePathSegment(value, fmt.Sprintf("%s key %q", configName, key))
}

func optionalString(raw map[string]any, key, fallback string) (string, error) {
	value, ok := raw[key]
	if !ok {
		return fallback, nil
	}
	text, ok := value.(string)
	if !ok || text == "" {
		return "", fmt.Errorf("%s key %q must be a non-empty string", configName, key)
	}
	return text, nil
}

func optionalRelativePath(raw map[string]any, key, fallback string) (string, error) {
	value, err := optionalString(raw, key, fallback)
	if err != nil || value == "" {
		return value, err
	}
	return validateRelativePath(value, fmt.Sprintf("%s key %q", configName, key))
}

func optionalRelativePathList(raw map[string]any, key string, fallback []string) ([]string, error) {
	value, ok := raw[key]
	if !ok {
		return fallback, nil
	}
	items, err := relativePathListFromMetadata(value)
	if err != nil {
		return nil, fmt.Errorf("%s key %q must be a list", configName, key)
	}
	paths := make([]string, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		path, err := validateRelativePath(item, fmt.Sprintf("%s key %q", configName, key))
		if err != nil {
			return nil, err
		}
		if isReservedRuntimePath(path) {
			return nil, fmt.Errorf("%s key %q uses reserved runtime path %q", configName, key, path)
		}
		if seen[path] {
			return nil, fmt.Errorf("%s key %q contains duplicate path %q", configName, key, path)
		}
		seen[path] = true
		paths = append(paths, path)
	}
	for i, left := range paths {
		for _, right := range paths[i+1:] {
			if pathsOverlap(left, right) {
				return nil, fmt.Errorf("%s key %q contains overlapping paths %q and %q", configName, key, left, right)
			}
		}
	}
	return paths, nil
}

func pathsOverlap(left, right string) bool {
	return left == right || isPathAncestor(left, right) || isPathAncestor(right, left)
}

func isPathAncestor(parent, child string) bool {
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}

func isReservedRuntimePath(path string) bool {
	first, _, _ := strings.Cut(path, string(filepath.Separator))
	switch first {
	case metadataDir, venvDir, ".cache":
		return true
	default:
		return false
	}
}

func relativePathListFromMetadata(value any) ([]string, error) {
	items, ok := asAnySlice(value)
	if !ok {
		return nil, fmt.Errorf("not a list")
	}
	paths := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok || text == "" {
			return nil, fmt.Errorf("not a path list")
		}
		paths = append(paths, text)
	}
	return paths, nil
}

func parseInstallSource(raw map[string]any) (installSource, error) {
	path, err := requireString(raw, "source")
	if err != nil {
		return installSource{}, err
	}
	return parseInstallSourceValue(path)
}

func parseInstallSourceValue(path string) (installSource, error) {
	path, err := validateSourcePath(path, fmt.Sprintf("%s key %q", configName, "source"))
	if err != nil {
		return installSource{}, err
	}
	if isPythonScriptSource(path) {
		return installSource{path: path, kind: sourceScript}, nil
	}
	return installSource{path: path, kind: sourcePackage}, nil
}

func parseCommands(value any, key string) ([][]string, error) {
	items, ok := asAnySlice(value)
	if !ok {
		return nil, fmt.Errorf("%s key %q must be a list", configName, key)
	}

	commands := make([][]string, 0, len(items))
	for _, item := range items {
		command, err := parseCommandItem(item)
		if err != nil {
			return nil, fmt.Errorf("%s key %q contains an invalid command", configName, key)
		}
		if len(command) == 0 {
			return nil, fmt.Errorf("%s key %q contains an empty command", configName, key)
		}
		commands = append(commands, command)
	}
	return commands, nil
}

func parseCommandItem(item any) ([]string, error) {
	if text, ok := item.(string); ok {
		return splitCommand(text)
	}
	parts, ok := asAnySlice(item)
	if !ok {
		return nil, fmt.Errorf("not a command")
	}
	command := make([]string, 0, len(parts))
	for _, part := range parts {
		text, ok := part.(string)
		if !ok {
			return nil, fmt.Errorf("not a string")
		}
		command = append(command, text)
	}
	return command, nil
}

func asAnySlice(value any) ([]any, bool) {
	switch typed := value.(type) {
	case []any:
		return typed, true
	case []string:
		items := make([]any, len(typed))
		for i, item := range typed {
			items[i] = item
		}
		return items, true
	case [][]string:
		items := make([]any, len(typed))
		for i, item := range typed {
			items[i] = item
		}
		return items, true
	default:
		return nil, false
	}
}

func splitCommand(text string) ([]string, error) {
	var fields []string
	var current strings.Builder
	var quote rune
	escaped := false
	inField := false

	flush := func() {
		fields = append(fields, current.String())
		current.Reset()
		inField = false
	}

	for _, r := range text {
		if escaped {
			current.WriteRune(r)
			escaped = false
			inField = true
			continue
		}
		if quote != 0 {
			switch {
			case r == quote:
				quote = 0
			case r == '\\' && quote == '"':
				escaped = true
			default:
				current.WriteRune(r)
			}
			inField = true
			continue
		}

		switch {
		case unicode.IsSpace(r):
			if inField {
				flush()
			}
		case r == '\'' || r == '"':
			quote = r
			inField = true
		case r == '\\':
			escaped = true
			inField = true
		default:
			current.WriteRune(r)
			inField = true
		}
	}

	if escaped {
		return nil, fmt.Errorf("unfinished escape")
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote")
	}
	if inField {
		flush()
	}
	return fields, nil
}

func validatePathSegment(value, label string) (string, error) {
	if filepath.IsAbs(value) || filepath.Base(value) != value || value == "." || value == ".." {
		return "", fmt.Errorf("%s must be a single path segment", label)
	}
	return value, nil
}

func looksLikePath(value string) bool {
	if filepath.IsAbs(value) {
		return true
	}
	for i := 0; i < len(value); i++ {
		if os.IsPathSeparator(value[i]) {
			return true
		}
	}
	return false
}

func validateRelativePath(value, label string) (string, error) {
	if filepath.IsAbs(value) {
		return "", fmt.Errorf("%s must be a relative path", label)
	}
	clean := filepath.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s must stay inside the checkout root", label)
	}
	return clean, nil
}

func validateSourcePath(value, label string) (string, error) {
	if filepath.IsAbs(value) {
		return "", fmt.Errorf("%s must be a relative path", label)
	}
	clean := filepath.Clean(value)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s must stay inside the source checkout", label)
	}
	return clean, nil
}

func isPythonScriptSource(source string) bool {
	return strings.EqualFold(filepath.Ext(source), ".py")
}

func configuredEntrypoint(ctx pinContext) string {
	if ctx.config != nil {
		return ctx.config.entrypoint
	}
	metadata, err := readCurrentMetadata(ctx)
	if err == nil && metadata != nil && metadata.string("entrypoint") != "" {
		return metadata.string("entrypoint")
	}
	return ctx.name
}

func requireConfig(ctx pinContext) (*config, error) {
	if ctx.config == nil {
		return nil, fmt.Errorf("no config available for %s; pass a repo path with %s", ctx.name, configName)
	}
	return ctx.config, nil
}

func configFromContextOrMetadata(ctx pinContext) (*config, error) {
	if ctx.config != nil {
		return ctx.config, nil
	}
	metadata, err := readCurrentMetadata(ctx)
	if err != nil {
		return nil, err
	}
	if metadata == nil {
		return nil, fmt.Errorf("no active release for %s", ctx.name)
	}
	return loadConfigFromMetadata(metadata)
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
