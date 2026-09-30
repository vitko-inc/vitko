#!/usr/bin/env python3
"""Fail when text that must stay internal appears in this public repository.

The deny-list holds SHA-256 hashes, not words, so this file and the list can be
public without revealing what they guard against. Each line of
denylist.sha256 is "<hash> i" (matched case-insensitively) or "<hash> c"
(matched exactly). Terms are compared as whole tokens: words, dotted or dashed
names and their parts, and two-word phrases.

Checks, in any combination:
  scan.py                      tracked files (git ls-files) and their paths
  scan.py --git-log RANGE      commit messages in RANGE (for example origin/main..HEAD, or --all)
  scan.py --text NAME          text on stdin (a pull request title and body, a branch name)

Findings print file:line and the hash of the term, never the term itself.
A justified exception goes in allowlist.txt as "<path glob> <hash>  # reason".
Standard library only; Python 3.8+.
"""
import argparse, fnmatch, hashlib, os, re, subprocess, sys

HERE = os.path.dirname(os.path.abspath(__file__))
TOKEN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._/@:-]*")
SEP = re.compile(r"([._/@:-])")
WORD = re.compile(r"[A-Za-z0-9]+")
MAX_PARTS = 6


def h(s):
    return hashlib.sha256(s.encode("utf-8")).hexdigest()


def load_deny():
    ci, cs = set(), set()
    with open(os.path.join(HERE, "denylist.sha256"), encoding="utf-8") as f:
        for line in f:
            line = line.split("#", 1)[0].strip()
            if not line:
                continue
            digest, mode = line.split()
            (cs if mode == "c" else ci).add(digest)
    return ci, cs


def load_allow():
    out = []
    path = os.path.join(HERE, "allowlist.txt")
    if os.path.exists(path):
        with open(path, encoding="utf-8") as f:
            for line in f:
                line = line.split("#", 1)[0].strip()
                if line:
                    glob, digest = line.split()
                    out.append((glob, digest))
    return out


def candidates(line):
    """Yield every token, every contiguous run of a token's parts, and every word pair."""
    for m in TOKEN.finditer(line):
        tok = m.group(0).rstrip(".:/-")
        parts = SEP.split(tok)  # words at even indexes, separators at odd
        words = parts[0::2]
        for i in range(len(words)):
            for j in range(i, min(len(words), i + MAX_PARTS)):
                yield "".join(parts[2 * i:2 * j + 1])
    words = WORD.findall(line)
    for a, b in zip(words, words[1:]):
        yield a + " " + b


def scan_text(name, text, ci, cs, allow):
    found = []
    for n, line in enumerate(text.splitlines(), 1):
        for c in set(candidates(line)):
            for digest in ((h(c.lower()),) if h(c.lower()) in ci else ()) + ((h(c),) if h(c) in cs else ()):
                if any(fnmatch.fnmatch(name, g) and d == digest for g, d in allow):
                    continue
                found.append((name, n, digest))
    return found


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--git-log", metavar="RANGE", help="scan commit messages in this range")
    ap.add_argument("--text", metavar="NAME", help="scan stdin, reported as NAME")
    ap.add_argument("--no-files", action="store_true", help="don't scan tracked files")
    a = ap.parse_args()
    ci, cs = load_deny()
    allow = load_allow()
    found = []
    if not a.no_files and not a.text:
        files = subprocess.run(["git", "ls-files", "-z"], check=True, capture_output=True).stdout.decode().split("\0")
        for path in filter(None, files):
            found += scan_text(path, path, ci, cs, allow)  # the path itself
            try:
                with open(path, encoding="utf-8") as f:
                    text = f.read()
            except (UnicodeDecodeError, IsADirectoryError, FileNotFoundError):
                continue
            found += scan_text(path, text, ci, cs, allow)
    if a.git_log:
        rng = ["--all"] if a.git_log == "--all" else [a.git_log]
        log = subprocess.run(["git", "log", "--format=%H%n%B%n%x00"] + rng, check=True, capture_output=True).stdout.decode()
        for entry in filter(str.strip, log.split("\0")):
            sha, _, body = entry.strip().partition("\n")
            found += scan_text("commit " + sha[:12], body, ci, cs, allow)
            # Author and committer names and emails are part of the public record too.
        idents = subprocess.run(["git", "log", "--format=%an <%ae>%n%cn <%ce>"] + rng, check=True, capture_output=True).stdout.decode()
        found += scan_text("commit identities", "\n".join(sorted(set(idents.splitlines()))), ci, cs, allow)
    if a.text:
        found += scan_text(a.text, sys.stdin.read(), ci, cs, allow)
    for name, n, digest in sorted(set(found)):
        print(f"{name}:{n}: internal term (sha256 {digest[:16]}). Remove it, or allowlist it with a reason.")
    if found:
        print(f"\n{len(set(found))} finding(s). See the note at the top of {os.path.relpath(__file__)}.", file=sys.stderr)
        return 1
    print("disclosure scan: clean")
    return 0


if __name__ == "__main__":
    sys.exit(main())
