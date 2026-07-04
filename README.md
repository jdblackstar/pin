# pin

`pin` installs Python command-line tools from a clean Git checkout into immutable
release directories, then exposes a stable current release directory for cron
jobs, agents, and other local automations.

The `0.1.0` scope is intentionally narrow: Python tools only.

## Install pin

Install the `pin` CLI with Homebrew:

```bash
brew install jdblackstar/tap/pin
```

## What pin Does

- reads a repo-local `pin.toml`
- verifies the checkout is clean and matches `origin/main`
- runs optional preflight commands
- builds a new release under `~/.local/share/<tool>/releases/<git-sha>/`
- injects optional mutable files from the source checkout into the release
- creates a Python virtual environment at `.venv/` inside that release
- verifies the candidate before activation
- atomically updates `current` and `previous` symlinks

That stable `current` directory is the path your cron job, launchd job, or agent
should use as its runtime checkout.

## Quickstart

Create a `pin.toml` in the repo that owns your Python automation:

```bash
pin init --name daily-report --source scripts/daily_report.py --verify "daily-report --help" /path/to/daily-report-repo
```

That writes:

```toml
name = "daily-report"
source = "scripts/daily_report.py"
branch = "main"
remote = "origin"
verify = [["daily-report", "--help"]]
```

Commit and push the repo, then install it:

```bash
pin update /path/to/daily-report-repo
```

Point automation at the stable current release:

```cron
15 8 * * * cd /Users/you/.local/share/daily-report/current && ./.venv/bin/daily-report
```

Future updates are the same command after merging to `main`:

```bash
pin check /path/to/daily-report-repo
pin update /path/to/daily-report-repo
```

If an update is bad, swap back to the previous release:

```bash
pin rollback daily-report
```

## Choosing `source`

Every `pin.toml` has one install target:

```toml
source = "scripts/daily_report.py"
```

Use a Python file when the tool is a single script or a small automation repo
that does not need packaging metadata. `pin` writes a console wrapper in the
release venv that runs that script from the archived checkout. Add
`requirements` when that script needs third-party dependencies.

```toml
source = "."
```

Use a directory when the repo is an installable Python package with a
`pyproject.toml` and a console script. `pin` installs that archived directory
into the release venv with `uv pip install .` when `uv` is available, or
`python -m pip install .` otherwise.

`entrypoint` is the generated command name inside `.venv/bin`. It defaults to
`name`, so set it only when the release-local command should have a different
name.

## Script Example

For a single Python script:

```toml
name = "daily-report"
source = "scripts/daily_report.py"
branch = "main"
remote = "origin"
preflight = [["python3", "-c", "from pathlib import Path; compile(Path('scripts/daily_report.py').read_text(), 'scripts/daily_report.py', 'exec')"]]
verify = [["daily-report", "--help"]]
```

`entrypoint` defaults to `name`, so the release-local command above is
`.venv/bin/daily-report`.

Then install or update it:

```bash
pin update /path/to/daily-report-repo
```

After a successful update, run it from:

```bash
cd ~/.local/share/daily-report/current
./.venv/bin/daily-report
```

## Python Package Example

For a Python project with a package entrypoint in `pyproject.toml`:

```toml
name = "staffmate"
source = "."
branch = "main"
remote = "origin"
preflight = [["uv", "run", "pytest"]]
verify = [["staffmate", "--help"]]
```

`entrypoint` defaults to `name`; set it only when the release-local console
script has a different name.

## Optional Requirements

A script source can install a requirements file into the release venv:

```toml
source = "scripts/daily_report.py"
requirements = "requirements.txt"
```

Keep this file committed. `pin` installs from the archived Git commit, not from
untracked local files.

`pin` keeps default `uv` and `pip` caches inside each release directory, so
agent and cron environments do not need write access to user-level cache
directories. If `UV_CACHE_DIR` or `PIP_CACHE_DIR` is already set, `pin` leaves it
alone.

## Runtime Injection

Use `inject` when a tool needs gitignored runtime files from the mutable source
checkout:

```toml
inject = [".env"]
```

Each path must be relative to the source checkout. During `pin update`, `pin`
creates a symlink at the same path inside the archived release checkout:

```text
~/.local/share/<tool>/releases/<git-sha>/.env -> /path/to/source/.env
```

This keeps code pinned to a Git SHA while secrets and local runtime config remain
rotatable in one place. Rollback changes the active code release, but it does not
restore old credentials. `pin update` and `pin verify` fail if a configured
injected file is missing or if the release no longer contains the expected
symlink.

Multiple files can be injected:

```toml
inject = [".env", "config/local.toml"]
```

Release directories are laid out like normal checkouts of the pinned commit, with
runtime files added alongside the repo contents:

```text
~/.local/share/<tool>/
  current -> releases/<git-sha>
  previous -> releases/<old-sha>
  releases/
    <git-sha>/
      pyproject.toml
      pin.toml
      package_or_scripts/
      .env -> /path/to/source/.env
      config/local.toml -> /path/to/source/config/local.toml
      .venv/
      .pin/release.json
```

## Commands

```bash
pin init [path]
pin status [tool_or_path]
pin check [tool_or_path]
pin update [tool_or_path]
pin verify [tool_or_path]
pin rollback [tool_or_path]
pin list
```

`tool_or_path` can be either a repo path containing `pin.toml` or an installed
tool name. Commands that need source state, such as `update` and `check`, need
the config from the repo path or from release metadata.

## Safety Model

`pin update` refuses to activate a release when:

- the source checkout is dirty
- the source branch is not the configured branch
- `HEAD` does not match the configured remote branch
- a configured injected file is missing
- preflight commands fail
- candidate verification fails

Failed candidate releases stay in place for inspection, but they are not made
current.
