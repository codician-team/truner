# CLI reference

`truner` installs, operates and removes an installation. Every command accepts these
global flags:

| Flag | Meaning |
|---|---|
| `--profile NAME` | AWS profile to use |
| `--region NAME` | AWS region (defaults to the region of the last install) |
| `--stack NAME` | installation (CloudFormation stack) name, default `truner` |
| `-y`, `--yes` | assume yes and never prompt (for scripts) |

Several installations can live side by side in one account, for example one per team or
one for untrusted repositories. Give each one its own `--stack` and `--label-prefix`.

## Setup

### install

Deploys the control plane, creates a private GitHub App and connects the two. Running it
again on an existing installation upgrades it and applies changed options. Aliases:
`setup`, `init`.

```bash
truner install [flags]
```

| Flag | Default | Meaning |
|---|---|---|
| `--github-owner ORG` | personal account | GitHub organization to create the app for |
| `--new-github-app` | | create a new GitHub App even if one is configured |
| `--label-prefix NAME` | `truner` | first `runs-on` label routed to this installation |
| `--alert-email ADDR` | | email for budget alerts and failures |
| `--daily-report` | | email a daily usage and cost report (needs `--alert-email`) |
| `--cache-days N` | `10` | days to keep magic cache entries and Docker S3 caches |
| `--log-days N` | `14` | days to keep machine and control plane logs |
| `--network create\|existing` | `create` | create a VPC, or use existing subnets |
| `--vpc-id ID` | | existing VPC (with `--network existing`) |
| `--public-subnets IDS` | | comma-separated subnets with internet access |
| `--private-subnets IDS` | | comma-separated subnets for `private=true` jobs |
| `--nat` | | private subnets behind a NAT gateway for a static egress IP |
| `--ssh-cidr CIDR` | | allow SSH to runners from this CIDR |
| `--runner-policy ARNS` | | comma-separated IAM policies to attach to runner machines |
| `--no-bake` | | do not pre-build optimized images after installing |
| `--no-ecr` | | do not create the ECR repository for Docker layer caching |

See [networking](networking.md) for the network options.

In scripts and AI agents, pass `--region` and `--github-owner` (or leave it out for a
personal account) and add `--yes`. Without a terminal the installer never prompts. It
prints lines starting with `ACTION REQUIRED`, each followed by a link the user has to
open, and it exits with a non-zero code on failure. If it is interrupted, run the same
command again: it continues where it stopped and reuses a pending GitHub link for up to
an hour. See [Install with an AI agent](ai-agents.md).

### login

Asks for an AWS access key, checks it and saves it as the AWS profile `truner`, which
later commands use automatically. It only runs in an interactive terminal. When an AI
agent runs the other commands for you, run `truner login` yourself, so the secret key
never passes through the agent.

```bash
truner login
```

### upgrade

Upgrades an installation to the version of this CLI. Install the new CLI first.

```bash
truner upgrade [--allow-downgrade]
```

A CLI older than the installation refuses, because deploying its version would take
away what came since. Install the latest CLI instead, or pass `--allow-downgrade` to go
back on purpose (`install` takes the same flag). Every command that reads the
installation warns when the CLI is older: an older CLI may not know its labels and
images, for example `micro` in `explain` and MicroVM images in `images list`.

### uninstall

Terminates all runner machines, deletes baked images, the data bucket and the
CloudFormation stack. It asks you to type the installation name to confirm, or
proceeds without asking with `--yes`.

```bash
truner uninstall [--keep-images]
```

Delete the GitHub App in your GitHub settings afterwards.

## Health

### status

The state of the installation: version, stack outputs (including the static egress IP
and the dashboard link), the label prefix jobs must use, running machines, the queue,
and today's jobs and spend.

```bash
truner status
```

### doctor

Checks the installation end to end and explains each problem: credentials, the stack,
the control plane, EC2 quotas (standard and GPU), the GitHub App's permissions, events and
installations, recent webhook deliveries and recent launch failures. Under **Jobs** it
prints the label prefix and lists queued jobs that no machine is coming for: jobs whose
labels carry another installation's prefix, and jobs whose machine ran another job with
the same labels. GitHub confirms each one is still queued.

```bash
truner doctor
```

## Jobs and costs

### jobs

Recent jobs with their machine, wait time, duration and cost.

```bash
truner jobs [--watch] [--repo owner/name] [--days 2] [--limit 30]
```

| Flag | Default | Meaning |
|---|---|---|
| `-w`, `--watch` | | refresh every 5 seconds |
| `--repo owner/name` | all | only this repository |
| `--days N` | `2` | look back this many days |
| `-n`, `--limit N` | `30` | number of jobs |

### costs

Estimated spend, and savings compared with GitHub-hosted runners of the same size.

```bash
truner costs [--by repo|workflow|type] [--days 30]
```

### logs

A runner machine's log (bootstrap, agent steps and runner output), or the control plane's
logs.

```bash
truner logs 123456789                  # a job id or runner name
truner logs --control-plane            # webhook, worker and janitor
truner logs --control-plane -f         # follow
truner logs --control-plane --since 6h 123456789
```

Machine logs are uploaded to S3 when the machine ends.

### explain

Shows which machine a `runs-on` label resolves to and, unless `--offline`, which instance
types match, with their current spot prices.

```bash
truner explain truner/cpu=8/ram=32
truner explain truner runner=16cpu-linux-arm64 spot=false
truner explain truner/runner=big --repo-config .github/truner.yml
```

| Flag | Meaning |
|---|---|
| `--offline` | do not contact AWS |
| `--json` | print the resolved spec as JSON |
| `--repo-config FILE` | repository config to apply (default `.github/truner.yml` if present) |

## Machines

### connect

Opens a shell on the machine running a job, through SSM Session Manager. It needs the AWS
CLI and the Session Manager plugin, and no open ports.

```bash
truner connect 123456789          # job id or instance id
truner connect --ssh 123456789    # SSH instead (ssh=true label and --ssh-cidr)
```

Add `keep=30m` to a job's labels to inspect its machine after the job finished.

### pools

Each [warm pool](pools.md)'s ready, preparing and in-use machines.

```bash
truner pools
```

### images

Optimized runner images (AMIs). See [images](images.md).

```bash
truner images list
truner images build ubuntu24-full --arch all    # x64, arm64 or all
truner images delete ami-0123456789abcdef0
```

### quota

Shows EC2 vCPU quotas and requests increases.

```bash
truner quota
truner quota --request 512
truner quota --gpu --request 64
```

## Cache

Inspect and clean the [magic cache](caching.md). All subcommands accept `--repo owner/name`.

```bash
truner cache ls --repo my-org/app
truner cache stats
truner cache rm node-cache-        # entries whose key starts with this prefix
truner cache purge --repo my-org/app
```

## Configuration

The account-wide [configuration](configuration.md).

```bash
truner config show
truner config edit                          # opens $EDITOR, validates, uploads
truner config apply truner.yml
truner config validate truner.yml
truner config validate --repo .github/truner.yml
```

## GitHub

```bash
truner github status     # the app, its installations and recent webhook deliveries
truner github install    # open the page to install the app on more repositories
```

## Other

```bash
truner version
truner completion bash|zsh|fish|powershell
```
