# Contributing

Thanks for helping! Issues and pull requests are welcome.

## Layout

```
cmd/truner          CLI entry point
cmd/truner-lambda   control plane Lambda (webhook, worker, janitor)
cmd/truner-agent    agent that runs on every runner machine
internal/cli        CLI commands (install, doctor, jobs, costs, …)
internal/deploy     CloudFormation template and stack deployment
internal/controlplane  webhook/worker/janitor logic, EC2 launching, pools, images
internal/agent      machine preparation, runner supervision, profiles/*.sh|ps1
internal/cacheproxy magic cache server (GitHub cache protocols → S3)
internal/config     labels → machine spec resolution, config files
internal/github     minimal GitHub App client
tools/build         release build (embeds agent + Lambda into the CLI)
docs/               user documentation
```

## Building

Go 1.26 or newer.

```bash
go build ./...
go test ./...
go run ./tools/build -targets host      # dist/truner_<os>_<arch> with embedded artifacts
```

A CLI built with plain `go build` has no embedded artifacts. For `truner install` from
a development build, either run `tools/build` first (it writes the artifacts into
`internal/assets/dist`, which `go build` then embeds) or point `TRUNER_ASSETS_DIR` at a
directory containing `truner-lambda.zip` and the `truner-agent-*` binaries. Development
installs version their artifacts by content hash, so every change is deployed.

## Integration tests

Control plane end-to-end tests run the real webhook, worker and janitor code against
DynamoDB Local, with fake EC2/SQS/SSM/Pricing and GitHub APIs:

```bash
docker run -d -p 8000:8000 amazon/dynamodb-local
TRUNER_DYNAMODB_LOCAL=http://localhost:8000 go test ./internal/controlplane -v
```

The magic cache is tested against the real `@actions/cache` toolkit:

```bash
mkdir /tmp/toolkit && cd /tmp/toolkit && npm init -y && npm i @actions/cache
TRUNER_NODE_E2E=/tmp/toolkit go test ./internal/cacheproxy -run NodeToolkit -v
```

The profile scripts can be tried in a container:

```bash
docker run --rm -v "$PWD/internal/agent/profiles:/p" ubuntu:24.04 \
  bash -c 'apt-get update && apt-get install -y curl sudo && useradd -m runner && bash /p/standard.sh'
```

## Guidelines

- Keep the control plane idempotent: every handler may run twice (webhook redelivery,
  SQS at-least-once delivery, overlapping janitor runs).
- Anything that launches or deletes AWS resources must tag or check `truner:stack`.
- User-facing errors should say what to do next.
- Run `gofmt`, `go vet ./...` and the tests before sending a PR.
