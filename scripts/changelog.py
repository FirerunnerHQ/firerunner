#!/usr/bin/env python3
"""changelog.py: unreleased changes as one file each, folded in at release.

Every PR that changes something a user can see adds its own file under
changelog.d/, so parallel PRs never edit the same lines of CHANGELOG.md
(they all used to append under "## [Unreleased]" and conflicted with each
other). A fragment holds one or more Keep a Changelog sections:

    ### Fixed

    - What changed, for the user.

Usage: scripts/changelog.py check            validate every fragment (exit 1 on a bad one)
       scripts/changelog.py show [--json]    print the unreleased changes, CHANGELOG.md's and the fragments'
       scripts/changelog.py release VERSION [DATE]
                                            move them under "## [VERSION] - DATE" (default today, UTC),
                                            update the compare links and delete the fragments

Exit status: 0 ok, 1 a check failed, 2 bad usage. Needs only python3.
"""
import datetime
import json
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CHANGELOG = os.path.join(ROOT, "CHANGELOG.md")
FRAGMENTS = os.path.join(ROOT, "changelog.d")
SECTIONS = ["Added", "Changed", "Deprecated", "Removed", "Fixed", "Security"]
REPO = "https://github.com/FirerunnerHQ/firerunner"


def usage(msg):
    print(f"changelog: {msg} (see scripts/changelog.py --help)", file=sys.stderr)
    sys.exit(2)


def parse_sections(text, where):
    """Map section -> list of entries (each a "- ..." line plus its indented
    continuation lines). Raises ValueError naming where on a bad layout."""
    out, current, entry = {}, None, None
    for n, line in enumerate(text.splitlines(), 1):
        if not line.strip():
            entry = None
            continue
        m = re.fullmatch(r"### (\S+)", line)
        if m:
            if m.group(1) not in SECTIONS:
                raise ValueError(f"{where}:{n}: unknown section {m.group(1)!r} (one of {', '.join(SECTIONS)})")
            current, entry = m.group(1), None
            out.setdefault(current, [])
            continue
        if current is None:
            raise ValueError(f"{where}:{n}: text before a '### <section>' heading")
        if line.startswith("- "):
            entry = [line]
            out[current].append(entry)
        elif line.startswith("  ") and entry is not None:
            entry.append(line)
        else:
            raise ValueError(f"{where}:{n}: expected '- ' to start an entry, or two spaces to continue one")
    for section, entries in out.items():
        if not entries:
            raise ValueError(f"{where}: section {section} has no entries")
    if not out:
        raise ValueError(f"{where}: no '### <section>' with entries")
    return {s: ["\n".join(e) for e in es] for s, es in out.items()}


def fragments():
    if not os.path.isdir(FRAGMENTS):
        return []
    names = sorted(n for n in os.listdir(FRAGMENTS) if n.endswith(".md") and n != "README.md")
    return [os.path.join(FRAGMENTS, n) for n in names]


def read_changelog():
    text = open(CHANGELOG).read()
    m = re.search(r"^## \[Unreleased\]\n(.*?)(?=^## \[)", text, re.S | re.M)
    if not m:
        raise ValueError("CHANGELOG.md: no '## [Unreleased]' section followed by a release")
    return text, m


def unreleased():
    """The unreleased entries by section: CHANGELOG.md's first, then each
    fragment's, in file-name order."""
    text, m = read_changelog()
    merged = {s: [] for s in SECTIONS}
    sources = []
    body = m.group(1).strip()
    if body:
        for s, es in parse_sections(body, "CHANGELOG.md [Unreleased]").items():
            merged[s] += es
    for path in fragments():
        rel = os.path.relpath(path, ROOT)
        for s, es in parse_sections(open(path).read(), rel).items():
            merged[s] += es
        sources.append(rel)
    return {s: es for s, es in merged.items() if es}, sources


def render(sections):
    parts = []
    for s in SECTIONS:
        if s in sections:
            parts.append(f"### {s}\n\n" + "\n".join(sections[s]))
    return "\n\n".join(parts)


def cmd_check():
    bad = 0
    for path in fragments():
        try:
            parse_sections(open(path).read(), os.path.relpath(path, ROOT))
        except ValueError as e:
            print(f"FAIL {e}")
            bad += 1
    try:
        unreleased()
    except ValueError as e:
        if not str(e).startswith("changelog.d/"):
            print(f"FAIL {e}")
            bad += 1
    if bad:
        sys.exit(1)
    print(f"ok   {len(fragments())} changelog fragment(s) valid")


def cmd_show(args):
    as_json = args == ["--json"]
    if args and not as_json:
        usage("show takes only --json")
    sections, sources = unreleased()
    if as_json:
        print(json.dumps({"sections": sections, "fragments": sources}, indent=2))
    else:
        print(render(sections) if sections else "(no unreleased changes)")


def cmd_release(args):
    if not 1 <= len(args) <= 2:
        usage("release needs VERSION [DATE]")
    version = args[0].removeprefix("v")
    if not re.fullmatch(r"\d+\.\d+\.\d+", version):
        usage(f"version {args[0]!r} is not MAJOR.MINOR.PATCH")
    date = args[1] if len(args) == 2 else datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%d")
    if not re.fullmatch(r"\d{4}-\d{2}-\d{2}", date):
        usage(f"date {date!r} is not YYYY-MM-DD")
    text, m = read_changelog()
    if f"## [{version}]" in text:
        print(f"changelog: {version} is already released", file=sys.stderr)
        sys.exit(1)
    sections, sources = unreleased()
    if not sections:
        print("changelog: nothing unreleased to release", file=sys.stderr)
        sys.exit(1)
    release = f"## [Unreleased]\n\n## [{version}] - {date}\n\n{render(sections)}\n\n"
    text = text[: m.start()] + release + text[m.end():]
    prev = re.search(r"^\[Unreleased\]: .*/compare/v([^.]+\.[^.]+\.[^.]+)\.\.\.HEAD$", text, re.M)
    if prev:
        text = text.replace(
            prev.group(0),
            f"[Unreleased]: {REPO}/compare/v{version}...HEAD\n[{version}]: {REPO}/releases/tag/v{version}",
        )
    open(CHANGELOG, "w").write(text)
    for rel in sources:
        os.remove(os.path.join(ROOT, rel))
    print(f"released {version} ({date}) from CHANGELOG.md and {len(sources)} fragment(s)")


def main(argv):
    if not argv:
        usage("need a command")
    if argv[0] in ("-h", "--help", "help"):
        print(__doc__.strip())
        sys.exit(0)
    cmd, args = argv[0], argv[1:]
    try:
        if cmd == "check":
            cmd_check()
        elif cmd == "show":
            cmd_show(args)
        elif cmd == "release":
            cmd_release(args)
        else:
            usage(f"unknown command {cmd!r}")
    except ValueError as e:
        print(f"changelog: {e}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main(sys.argv[1:])
