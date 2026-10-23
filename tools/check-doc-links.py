#!/usr/bin/env python3
"""Check relative links in Markdown files: every target file must exist.

Used by the docs-only CI workflow. External links are not fetched (CI has no
business depending on third-party sites); anchors are not checked.
Usage: tools/check-doc-links.py [root]
"""
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else ".").resolve()
link = re.compile(r"\[[^\]]*\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)")
skip_dirs = {"node_modules", ".git", "test-results", "playwright-report"}
bad = []
files = 0
for md in root.rglob("*.md"):
    if any(part in skip_dirs for part in md.parts):
        continue
    files += 1
    text = md.read_text(encoding="utf-8", errors="replace")
    # Ignore fenced code blocks: they contain example syntax, not links.
    text = re.sub(r"```.*?```", "", text, flags=re.S)
    text = re.sub(r"``.*?``|`[^`\n]*`", "", text)  # inline code spans
    for target in link.findall(text):
        if re.match(r"^[a-z][a-z0-9+.-]*:", target) or target.startswith("#"):
            continue
        path = target.split("#", 1)[0]
        if not path:
            continue
        resolved = (md.parent / path).resolve()
        if not resolved.exists():
            bad.append(f"{md.relative_to(root)}: {target}")
for b in bad:
    print("broken link:", b)
print(f"checked {files} Markdown files, {len(bad)} broken relative links")
sys.exit(1 if bad else 0)
