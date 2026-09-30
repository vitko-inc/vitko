# vitko

The command-line tool for [Vitko](https://runners.vitko.inc). It works the same for people and for coding agents: text on a terminal, JSON everywhere else, stable error codes and exit codes, and a machine-readable description of every command.

Today it covers [Vitko Runners](https://runners.vitko.inc), GitHub Actions runners at a third of GitHub's price:

```console
$ vitko runners pricing                          # the price list, dated
$ vitko runners estimate usage-report.csv        # what your CI would cost, from GitHub's usage report
$ vitko runners repos switch --dry-run           # point your workflows at Vitko runners
```

## Install

```sh
curl -fsSL https://github.com/vitko-inc/vitko/releases/latest/download/install.sh | sh
```

The script is non-interactive. It checks the SHA-256 checksum of the download, checks the Sigstore signature when [cosign](https://docs.sigstore.dev) is installed, and installs to `~/.local/bin`. Settings: `VITKO_VERSION` (for example `v0.1.0`), `VITKO_INSTALL_DIR`, `VITKO_VERIFY=always|auto|never`, and `VITKO_OUTPUT=json` for a JSON result.

Other ways:

- **Homebrew** (macOS and Linux): `brew install vitko-inc/tap/vitko`
- **GitHub Actions**: `uses: vitko-inc/setup-vitko@v1`
- **Go**: `go install github.com/vitko-inc/vitko/cmd/vitko@latest`
- **Download**: archives for Linux and macOS (x86-64 and arm64) on the [releases page](https://github.com/vitko-inc/vitko/releases).

### Verify a download by hand

Every release has `checksums.txt`, a Sigstore bundle for it (`checksums.txt.sigstore.json`), an SBOM per archive, and GitHub build provenance for each archive.

```sh
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/vitko-inc/vitko/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum --ignore-missing -c checksums.txt
gh attestation verify vitko_linux_amd64.tar.gz --repo vitko-inc/vitko
```

## Commands

| Command | What it does |
|---|---|
| `vitko runners pricing` | The public price list: $0.002 per minute per slot (up to 2 vCPU / 8 GB), billed per second, 2,000 free minutes a month, and the dated GitHub price it is compared with. |
| `vitko runners estimate <file>...` | Reads GitHub's usage report (CSV, or the billing usage API's JSON) and shows what the Linux x64 minutes would cost on Vitko, next to what GitHub charged. Runs on your machine; nothing is uploaded. `--from-github <org>` fetches the report with the GitHub CLI. |
| `vitko runners repos switch [<path>]` | Changes `runs-on: ubuntu-latest` / `ubuntu-24.04` to `vitko-ubuntu-24.04` in `.github/workflows`, keeping comments and formatting. Lists what it leaves alone and why. `--dry-run` shows the diff, `--check` exits 10 if anything would change, `--pr` opens a pull request with your own `git` and `gh` sign-in. |
| `vitko login`, `vitko logout`, `vitko whoami` | Sign in from a terminal (open a link, check the code, approve), see who you are, sign out. |
| `vitko orgs list` | The organizations you can use. |
| `vitko tokens list/create/revoke` | Organization API tokens for agents and scripts. Creating and revoking needs an organization admin who signed in within the last 12 hours. |
| `vitko runners jobs list/show` | Jobs that ran on Vitko runners, newest first, with filters and paging (`--all`). |
| `vitko runners usage [--month YYYY-MM]` | A month's minutes and cost, next to GitHub's list price for the same jobs. |
| `vitko runners limits show/set` | The spend limit and jobs at once. `set` states the target value; `--dry-run` checks it without changing anything. |
| `vitko runners limits ceiling show/set` | Ceilings set by an organization admin: API tokens and CI jobs can set limits at or below them, and with no ceiling they can only lower limits. |
| `vitko help [--json]` | Every command, flag, output schema, error code and exit code. |
| `vitko schema list`, `vitko schema show <id>` | The JSON Schemas of the output. |
| `vitko config list/get/set` | Settings (`output`, `org`), stored in `~/.config/vitko/config.json`. |
| `vitko doctor` | Checks that vitko and the tools it uses (`git`, `gh`) are ready. |
| `vitko version` | The version. |

Where to get GitHub's usage report: **Settings → Billing and licensing → Usage → Get usage report** (an organization owner or billing manager). The summarized report is enough.

### Switching a repository

```console
$ vitko runners repos switch --dry-run
--- a/.github/workflows/ci.yml
+++ b/.github/workflows/ci.yml
@@ -3,7 +3,7 @@
 jobs:
   test:
-    runs-on: ubuntu-latest
+    runs-on: vitko-ubuntu-24.04
     steps:
       - uses: actions/checkout@v4

Would switch 1 job(s) to vitko-ubuntu-24.04. Nothing was written.
```

Jobs that are left alone, each with a reason: `runs-on` expressions and matrices, other operating systems and Arm, other Ubuntu versions, self-hosted runners, runner groups, custom or larger-runner labels, and workflows called from other repositories.

## Signing in

vitko uses the first of these it finds:

1. **`VITKO_TOKEN`**: an organization API token (`vitko_pat_…`) from `vitko tokens create`. It works in one organization, with the scopes it was given. There is no `--token` flag, because flags end up in shell history and process lists.
2. **Inside a GitHub Actions job**, no secret is needed. vitko exchanges the job's own ID token for a 15-minute one; add `permissions: id-token: write` to the job. By default a job can read its own repository's jobs. An organization admin can allow more with a CI trust rule.
3. **`vitko login`**: your own sign-in, kept in `~/.config/vitko/credentials.json` (readable only by you) and renewed automatically. `vitko login --with-token` stores an API token instead.

Pick an organization with `--org <login>`, `VITKO_ORG`, or `vitko config set org <login>`. If you belong to only one, it's used automatically.

## For agents and scripts

- **Output**: `--output text|json|ndjson` (or `VITKO_OUTPUT`). The default is `text` on a terminal and `json` otherwise, so an agent gets JSON without asking. `--fields a,b.c` keeps only the fields you need.
- **Schemas**: every JSON document has a `schema` field such as `vitko.runners.estimate/v1`. New fields can appear at any time; a breaking change bumps the version. Enums are open: expect values you don't know. `vitko schema show <id>` prints the JSON Schema.
- **Money** is in integer micros of USD (1 USD = 1,000,000) with a `currency` field. Unknown values are `null` with a `*_unavailable_reason`, never `0`.
- **Errors** go to stderr as `vitko.error/v1` in JSON modes, with a stable `code`, a `hint`, and a `fix.command` when a command would fix it.
- **Never prompts.** Missing input is a usage error (exit code 2).
- **`--dry-run`** on every command that changes something.

| Exit code | Meaning |
|---|---|
| 0 | Success |
| 1 | Unexpected failure |
| 2 | Usage: bad flag or argument, missing input, or a required tool is missing |
| 3 | Not signed in (for example the GitHub CLI) |
| 4 | Not allowed |
| 5 | Not found |
| 6 | Conflict with the current state, for example a stale `--if-revision` |
| 7 | A spend or concurrency limit, or its ceiling, stopped it |
| 8 | Temporarily unavailable; safe to retry |
| 10 | `--check`: changes would be made |

`vitko runners <name>` also runs a program called `vitko-runners-<name>` from your `PATH`, passing the output mode in `VITKO_OUTPUT`.

## Develop

```sh
go test ./...                                 # unit, golden and schema tests
go test ./internal/commands -update           # rewrite golden files after an intended change
python3 .github/disclosure/scan.py            # the content check CI runs
```

The command list lives in one place, [`internal/commands/commands.go`](internal/commands/commands.go). Help text, `vitko help --json` and the tests are driven from it.

## License

[Apache-2.0](LICENSE)
