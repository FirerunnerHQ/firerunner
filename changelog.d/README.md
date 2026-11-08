# Unreleased changes

Each change a user can see adds one file here, in the same PR, rather than a
line in `CHANGELOG.md`. Parallel PRs then never edit the same lines. Name the
file after the change, such as `console-log-cap.md`, and write it in the
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) shape:

```markdown
### Fixed

- What changed for the user, not the commit.
```

The sections are Added, Changed, Deprecated, Removed, Fixed and Security. A
file may hold more than one.

```sh
scripts/changelog.py check              # validate (make check runs it)
scripts/changelog.py show [--json]      # everything unreleased, merged
scripts/changelog.py release 0.1.9      # fold it into CHANGELOG.md, delete these files
```
