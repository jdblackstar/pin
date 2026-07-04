# pin

`pin` installs Python command-line tools from a clean Git checkout into immutable
release directories, then exposes a stable executable path for cron jobs, agents,
and other local automations.

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
- creates a Python virtual environment for that release
- verifies the candidate before activation
- atomically updates `current` and `previous` symlinks
- links `~/.local/bin/<entrypoint>` to the active release

That stable entrypoint is the path your cron job, launchd job, or agent should
call.

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
link = true
```

Commit and push the repo, then install it:

```bash
pin update /path/to/daily-report-repo
```

Point automation at the stable command:

```cron
15 8 * * * /Users/you/.local/bin/daily-report
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

`entrypoint` is the stable command name. It defaults to `name`, so set it only
when the command you want automation to call has a different name.

## Script Example

For a single Python script:

```toml
name = "daily-report"
source = "scripts/daily_report.py"
branch = "main"
remote = "origin"
preflight = [["python3", "-c", "from pathlib import Path; compile(Path('scripts/daily_report.py').read_text(), 'scripts/daily_report.py', 'exec')"]]
verify = [["daily-report", "--help"]]
link = true
```

`entrypoint` defaults to `name`, so the stable command above is
`~/.local/bin/daily-report`.

Then install or update it:

```bash
pin update /path/to/daily-report-repo
```

After a successful update, run it from:

```bash
~/.local/bin/daily-report
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
link = true
```

`entrypoint` defaults to `name`; set it only when the package console script has
a different name.

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
- preflight commands fail
- candidate verification fails
- the stable entrypoint path already exists as an unmanaged file

Failed candidate releases stay in place for inspection, but they are not made
current.
