"""Fresh-eyes audit of Shark BiteTorrent by Claude, run from the "Audit" workflow.

Sends the source tree and the changes since the last release in one request
and writes the findings to report.md. Claude gets no tools: it reads what is
sent and writes text, nothing else.

    ANTHROPIC_API_KEY=... python scripts/audit.py [--focus "extra instructions"]
"""

import argparse
import os
import re
import subprocess
import sys

MODEL = "claude-opus-5-5"
# Per million tokens, for the cost line in the report.
PRICE_IN, PRICE_OUT = 4.00, 20.00

INCLUDE = re.compile(
    r"\.(go|svelte|ts|js|css|html|yml|yaml|json|md|mod|nsi|nsh|plist|xml|sh|ps1)$"
)
EXCLUDE = re.compile(
    r"^(frontend/(dist|wailsjs|node_modules)/|build/bin/|third_party/)"
    r"|package-lock\.json$|^CHANGELOG\.md$"
)


def git(*args):
    return subprocess.run(
        ["git", *args], check=True, capture_output=True, text=True, encoding="utf-8"
    ).stdout


def audit_brief():
    # The brief lives in RELEASING.md so local and API audits ask the same thing.
    text = open("RELEASING.md", encoding="utf-8").read()
    m = re.search(r"## Audit brief\n.*?^> (.+?)$", text, re.S | re.M)
    if not m:
        sys.exit("RELEASING.md has no '## Audit brief' quote")
    return m.group(1)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--focus", default="", help="extra instructions for this audit")
    ap.add_argument("--out", default="report.md")
    ap.add_argument("--dry-run", action="store_true", help="show the size, send nothing")
    args = ap.parse_args()

    # From the last release, not the last beta (v0.3.0-beta.2): a release's
    # audit must see everything since the previous release.
    tag = git("describe", "--tags", "--abbrev=0", "--match", "v*", "--exclude", "*-*").strip()
    head = git("rev-parse", "--short", "HEAD").strip()
    diff = git("diff", f"{tag}..HEAD")

    files = [
        f for f in git("ls-files").splitlines() if INCLUDE.search(f) and not EXCLUDE.search(f)
    ]
    parts = []
    for f in files:
        with open(f, encoding="utf-8", errors="replace") as fh:
            parts.append(f'<file path="{f}">\n{fh.read()}\n</file>')

    prompt = (
        f"{audit_brief()}\n\n"
        f"The last release is {tag}; HEAD is {head}. Review the changes since {tag} "
        "closely, and use the rest of the repository to judge them and to find "
        "older problems the changes touch or expose. Vendored code in third_party/ "
        "and generated frontend bindings were left out.\n\n"
        "Write the report in Markdown: a short summary, then findings ordered by "
        "severity (file:line, scenario, fix), then the areas with nothing found, "
        "then what you did not review."
    )
    if args.focus:
        prompt += f"\n\nAlso, for this audit: {args.focus}"

    content = (
        f"<diff base=\"{tag}\" head=\"{head}\">\n{diff or '(no changes)'}\n</diff>\n\n"
        + "\n\n".join(parts)
        + f"\n\n{prompt}"
    )
    if args.dry_run:
        # About 4 characters per token for code.
        est = len(content) / 4
        print(f"{tag}..{head}: {len(files)} files, {len(content)} chars, "
              f"~{est:.0f} tokens in (~${est / 1e6 * PRICE_IN:.2f} before output)")
        return

    import anthropic  # only needed for a real run

    client = anthropic.Anthropic()
    with client.beta.messages.stream(
        model=MODEL,
        max_tokens=64000,
        thinking={"type": "adaptive"},
        output_config={"effort": "high"},
        betas=["server-side-fallback-2026-07-01"],
        extra_body={"fallbacks": "default"},
        messages=[{"role": "user", "content": content}],
    ) as stream:
        msg = stream.get_final_message()

    text = "".join(b.text for b in msg.content if b.type == "text")
    u = msg.usage
    cost = u.input_tokens / 1e6 * PRICE_IN + u.output_tokens / 1e6 * PRICE_OUT
    footer = (
        f"\n\n---\nAudited {tag}..{head} ({len(files)} files) with {msg.model}. "
        f"Tokens: {u.input_tokens} in, {u.output_tokens} out (about ${cost:.2f}). "
        f"Stop reason: {msg.stop_reason}."
    )

    with open(args.out, "w", encoding="utf-8") as fh:
        fh.write(f"# Audit of {tag}..{head}\n\n{text}{footer}\n")
    print(footer.strip())

    if msg.stop_reason == "refusal":
        details = getattr(msg, "stop_details", None)
        sys.exit(f"Claude declined the audit: {details}")
    if msg.stop_reason == "max_tokens":
        sys.exit("The report was cut off at max_tokens; it is incomplete")


if __name__ == "__main__":
    main()
