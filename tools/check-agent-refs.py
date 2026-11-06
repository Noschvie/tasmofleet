#!/usr/bin/env python3
"""Check that the agent guide names only things that exist (#526).

CLAUDE.md and docs/dev/*.md are what an agent reads before it works. Every file, directory,
make target and doc they name in backticks (and every package line of a fenced architecture
map) must exist, so the guide cannot drift away from the repository unnoticed.
Checked: `make <target>` against the Makefile; path-like words (a first segment that is a
top-level entry of the repository, or a file name with a known extension) against the file
tree, globs allowed, bare file names anywhere in the tree. Words with placeholders
(<x>, $x, NNNN, X.Y.Z) and the generated files in GENERATED are skipped.
Usage: tools/check-agent-refs.py [root]
"""
import fnmatch
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else ".").resolve()
SKIP_DIRS = {".git", "node_modules", "test-results", "playwright-report"}
GENERATED = {"mutation-report.json", "coverage.out", "_test.go"}  # written by a run, never committed; a suffix, not a file
EXT = r"(?:md|ya?ml|sh|py|go|templ|json|mjs|sql|txt|csv)"

docs = [p for p in [root / "CLAUDE.md", *sorted((root / "docs" / "dev").glob("*.md"))] if p.is_file()]
if not docs:
    print("no CLAUDE.md or docs/dev/*.md found under", root)
    sys.exit(1)

tree = []  # all file paths relative to root, posix
for p in root.rglob("*"):
    rel = p.relative_to(root)
    if SKIP_DIRS & set(rel.parts):
        continue
    tree.append(rel.as_posix())
tree_set = set(tree)
top = {t.split("/")[0] for t in tree}
basenames = {t.rsplit("/", 1)[-1] for t in tree}

targets = set()
makefile = root / "Makefile"
if makefile.is_file():
    for line in makefile.read_text(encoding="utf-8").splitlines():
        m = re.match(r"^([A-Za-z0-9_.%/-]+(?:\s+[A-Za-z0-9_.%/-]+)*)\s*::?(?!=)", line)
        if m and not line.startswith(("\t", "#")) and not re.match(r"^[A-Z_]+\s*[:?+]?=", line):
            targets.update(m.group(1).split())
        if line.startswith(".PHONY:"):
            targets.update(line[len(".PHONY:"):].split())


def exists(word):
    word = word.rstrip("/")
    if any(c in word for c in "*?["):
        return any(fnmatch.fnmatch(t, word) for t in tree) or any(
            fnmatch.fnmatch(b, word) for b in basenames
        )
    if "/" not in word:
        return word in basenames or word in top
    return word in tree_set or any(t.startswith(word + "/") for t in tree)


def path_like(word):
    if re.search(r"[<>${}]|NNNN|X\.Y\.Z|\.\.\.|\\", word) or word in GENERATED:
        return False
    if "/" in word:
        first = word.split("/")[0]
        return first in top or bool(re.search(r"\." + EXT + r"$", word) and "." not in first)
    return bool(re.fullmatch(r"[\w.*-]+\." + EXT, word)) and not word.startswith("*")


bad = []


def check_words(doc, text):
    words = text.split()
    for i, w in enumerate(words):
        w = w.strip("\"'(),;:")
        if w == "make" and i + 1 < len(words):
            t = words[i + 1].strip("\"'(),;`")
            if re.fullmatch(r"[a-z][a-z0-9-]*", t) and t not in targets:
                bad.append(f"{doc}: make target `{t}` is not in the Makefile")
            continue
        w = re.sub(r":\d+$", "", w.rstrip("."))
        if path_like(w) and not exists(w):
            bad.append(f"{doc}: `{w}` does not exist")


for doc in docs:
    rel = doc.relative_to(root).as_posix()
    text = doc.read_text(encoding="utf-8", errors="replace")
    in_fence = False
    for line in text.splitlines():
        if line.startswith("```"):
            in_fence = not in_fence
            continue
        if in_fence:
            first = line.split(" ")[0].rstrip(",")
            if re.match(r"^(cmd|internal)/", first):  # a package line of the architecture map
                check_words(rel, first)
            continue
        for span in re.findall(r"`([^`\n]+)`", line):
            check_words(rel, span)

for b in sorted(set(bad)):
    print("broken reference:", b)
print(f"checked {len(docs)} agent docs, {len(set(bad))} broken references")
sys.exit(1 if bad else 0)
