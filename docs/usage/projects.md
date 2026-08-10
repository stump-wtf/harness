---
title: "Projects & compose"
sidebar_position: 5
---

# Projects & compose

A **project** is a directory whose root carries a `harness.toml`. It's
Harness's equivalent of Docker Compose: declare the harnesses that make up the
project, then bring them up as a set with `harness up`, tear them down with
`harness down`, and list them with `harness ps` (ADR-0009 / SPEC-0004).

## Project file

A project reuses the `[harness.*]` schema. It **must not** contain `[server]` or
`[profile.*]` tables (those are global-only concerns). An optional `[project]`
table names the project:

```toml
[project]
name = "my-project"

[harness.api]
cmd = "uvicorn"
args = ["app.main:app"]
workdir = "."
description = "the API"
enabled = true
```

Relative `workdir` values resolve against the **project root**, not the daemon's
working directory.

## Discovery

`harness up`, `down`, and `ps` discover the enclosing project by walking **up**
from the current directory to the first ancestor `harness.toml` (stopping at
your home directory). The daemon's own global config file is never adopted as a
project file.

## The verbs

```sh
harness up                # bring the enclosing project up, detached
harness down [PROJECT]    # stop & deregister every harness of the project
harness ps                # inside a project: list only this project's harnesses
```

- `up` reconciles the running set to the project file, then prints a one-shot
  status table and returns to the shell — the harnesses keep running in the
  background.
- `down` stops and **deregisters** the project's harnesses so the daemon
  retains no record, and never touches your global config. Pass an explicit
  `[PROJECT]` to tear down a project whose file you've already deleted.

## Project-scoped names

Inside a project, a bare name to `describe`/`logs`/`start`/`stop`/`restart`/
`attach` resolves to `<project>/<name>` — again, purely lexically, with no
fallback to a global harness:

```sh
harness start api         # == harness start my-project/api
harness stop api
harness logs api
```

A name containing `/` is already fully qualified and passes through untouched.
Outside a project, `ps` is a plain alias for `list`, and all the same verbs keep
their global meaning.
