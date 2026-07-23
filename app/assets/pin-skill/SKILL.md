---
name: pin
description: Install, update, verify, run, inspect, and roll back Python command-line tools managed by PIN. Use when a repository has pin.toml, when the user asks to pin a Python tool or automation, or when working with a PIN-managed runtime.
---

# Use PIN

PIN builds immutable Python tool releases from clean Git commits and exposes the active release through a stable `current` directory.

## Inspect first

1. Run `pin --version` and `pin --help` if the available command surface is unclear.
2. Look for `pin.toml` in the repository root and read it before choosing a command.
3. Use the repository path for source-oriented operations so PIN reads the current checked-in configuration.

## Install or update a repository

1. Confirm the checkout is clean, on the branch configured by `pin.toml`, and synchronized with its configured remote.
2. Run `pin check <repo-path>` to compare the checkout with the installed release.
3. Run `pin update <repo-path>` to build, verify, and atomically activate the release.
4. Run `pin verify <tool-name>` after activation.

If `pin.toml` is missing and the user wants to prepare the repository, inspect the Python package or script entrypoint and use `pin init --help` to construct the appropriate `pin init` command. Show the proposed configuration before creating it when important values are ambiguous.

## Operate an installed tool

- Inspect it with `pin status <tool-name>`.
- Run it with `pin run <tool-name> -- <args...>`; omit `--` when there are no tool arguments.
- List installed tools with `pin list`.
- Recover from a bad activation with `pin rollback <tool-name>`, then run `pin verify <tool-name>`.

Do not edit files inside a managed release. Make source changes in the repository, commit them, and activate a new release with `pin update`.
