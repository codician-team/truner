# AGENTS.md

Instructions for AI coding agents working on the truner source code.

**Asked to install or set up truner for a user, not to change its code?** Follow
[docs/ai-agents.md](docs/ai-agents.md) instead.

## Project

truner runs GitHub Actions jobs on ephemeral EC2 machines in the user's AWS account. It
has three Go programs and a website:

| Path | What |
|---|---|
| `cmd/truner`, `internal/cli` | the CLI (install, doctor, jobs, …) |
| `cmd/truner-lambda`, `internal/controlplane` | webhook, worker and janitor Lambda functions |
| `cmd/truner-agent`, `internal/agent` | the agent on every runner machine, plus `profiles/*.sh` |
| `internal/cacheproxy`, `internal/cachestore` | the magic cache (GitHub cache protocols on S3) |
| `internal/config`, `internal/labels` | `runs-on` labels and config files resolved into a machine spec |
| `internal/deploy/template.yaml` | the CloudFormation stack |
| `docs/` | user docs (plain Markdown) and the VitePress website |

`CONTRIBUTING.md` has more detail.

## Commands

Go 1.26 or newer. Node.js 22 or newer for the website only.

```bash
go build ./...
go vet ./... && GOOS=linux go vet ./... && GOOS=windows go vet ./...
test -z "$(gofmt -l cmd internal tools)"
go test ./...

# Control plane integration tests (real handlers, DynamoDB Local, fake AWS and GitHub)
docker run -d --rm --name truner-ddb -p 8000:8000 amazon/dynamodb-local
TRUNER_DYNAMODB_LOCAL=http://localhost:8000 go test ./internal/controlplane/

cfn-lint internal/deploy/template.yaml
go run ./tools/build -targets host          # CLI with embedded agent and Lambda in dist/
cd docs && npm ci && npm run build          # website; fails on dead links
```

CI (`.github/workflows/ci.yml`) runs all of these. Run the ones that cover your change
before you finish.

## Rules

- **Control plane handlers must be idempotent.** Webhooks are redelivered, SQS delivers
  at least once and janitor runs overlap.
- Anything that launches, changes or deletes AWS resources must set or check the
  `truner:stack` tag. IAM in the template is scoped to it.
- The CLI never prompts without a terminal (`ui.ask` returns the default). Actions the
  user must take are printed with `ui.action`, which starts the line with
  `ACTION REQUIRED`. AI agents rely on that prefix, so keep it.
- Errors say what to do next, for example the command that fixes the problem.
- Keep `docs/*.md` readable on GitHub. VitePress containers (`::: tip`) are fine.
  Vue components belong only in `docs/index.md` and `docs/.vitepress/`.
- No page in `docs/` may be named `agents.md`. On case-insensitive file systems agents
  would read it as an `AGENTS.md` for that folder.
- When you add a docs page, add it to `sidebar` and `descriptions` in
  `docs/.vitepress/config.mts`. That also puts it into `llms.txt`.
- Match the style of the surrounding code: small packages, few comments that explain why,
  standard library first.
