# CLI reference

`truner` installs, operates and removes an installation. Every command accepts these
global flags:

| Flag | Meaning |
|---|---|
| `--profile NAME` | AWS profile to use. Without it, and without `AWS_PROFILE` or `AWS_ACCESS_KEY_ID`, the profile of the last `install` or `login` |
| `--region NAME` | AWS region. Defaults to `AWS_REGION` or `AWS_DEFAULT_REGION`, then the region of the last `install` or `login`, then the profile's region, then `us-east-1` |
| `--stack NAME` | installation (CloudFormation stack) name. Defaults to `TRUNER_STACK`, then the installation of the last `install`, then `truner` |
| `-y`, `--yes` | assume yes and never prompt (for scripts) |

The CLI keeps the last installation, region and profile in `~/.truner/config.json`.

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
| `--cache-days N` | `10` | days to keep magic cache entries and Docker S3 caches (at least 1) |
| `--log-days N` | `14` | days to keep machine and control plane logs: 1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180 or 365 |
| `--network create\|existing` | `create` | create a VPC, or use existing subnets |
| `--vpc-id ID` | | existing VPC (with `--network existing`) |
| `--public-subnets IDS` | | comma-separated subnets with internet access (with `--network existing`) |
| `--private-subnets IDS` | | comma-separated subnets for `private=true` jobs (with `--network existing`) |
| `--nat` | | private subnets behind a NAT gateway for a static egress IP (about $32 per month) |
| `--ssh-cidr CIDR` | | allow SSH to runners from this CIDR |
| `--runner-policy ARNS` | | comma-separated IAM policy ARNs to attach to runner machines and MicroVMs |
| `--no-bake` | | do not pre-build optimized images after installing |
| `--no-ecr` | | do not create the ECR repository for Docker layer caching |
| `--no-cli-update` | | deploy this CLI's version without updating the CLI first (see [upgrade](#upgrade)) |
| `--allow-downgrade` | | deploy this CLI's version even if the installation runs a newer one |

On an existing installation, only the options you pass change. The others keep their
values. See [networking](networking.md) for the network options.

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

Updates the CLI to the latest release, then the installation to it.

```bash
truner upgrade [--cli-only] [--no-cli-update] [--allow-downgrade]
```

| Flag | Meaning |
|---|---|
| `--cli-only` | update only the CLI, not the installation |
| `--no-cli-update` | deploy this CLI's version without updating the CLI first |
| `--allow-downgrade` | deploy this CLI's version even if the installation runs a newer one |

- When a newer release is out, the CLI downloads it, checks it against the release's
  checksums, replaces itself and runs the same command again with the new version.
  `install` does the same. It replaces the file it runs from, so it needs write access
  to that directory. If the update fails, the CLI prints an `ACTION REQUIRED` line with
  the install script's command and changes nothing. Install the latest CLI that way, or
  pass `--no-cli-update` to deploy the CLI's own version.
- Step 1 says what happens to the installation, for example
  `it will be upgraded from v0.1.3 to v0.1.5`, and the end says what happened, for
  example `Installation "truner" is upgraded from v0.1.3 to v0.1.5.`, with links to the
  release notes, the issue tracker and the commit log. It warns when the control plane
  then reports another version than the one deployed.
- `upgrade` fails when there is no installation, and it does not start image builds.
  Options such as `--alert-email` belong to `install`, which also upgrades.
- A CLI older than the installation refuses and changes nothing, because deploying its
  version would take away what came since. Pass `--allow-downgrade` to go back on purpose
  (`install` takes the same flag).
- The installation is locked with a CloudFormation stack policy that denies every
  update. The CLI lifts it for its own update only. A CLI older than v0.1.5 does not, so
  its update fails and rolls back without changing anything. The AWS identity that runs
  `install` and `upgrade` needs `cloudformation:SetStackPolicy`, and `doctor` uses
  `cloudformation:GetStackPolicy` to report installations that are not locked yet. Run
  `truner upgrade` once to lock those.

Every command that reads the installation warns when the CLI is older: an older CLI may
not know its labels and images, for example `micro` in `explain` and MicroVM images in
`images list`. `truner version` and `truner doctor` say when a newer release is out.

### uninstall

Terminates all runner machines and MicroVMs, deletes the MicroVM images, baked images,
the data bucket, the CloudFormation stack and the GitHub App's credentials in SSM. When
no other installation is left in the region, it also deletes the artifacts bucket. It
asks you to type the installation name to confirm, or proceeds without asking with
`--yes`.

```bash
truner uninstall [--keep-images]
```

`--keep-images` keeps the baked AMIs. MicroVM images are always deleted, because they
cannot run without the stack's roles. Delete the GitHub App in your GitHub settings
afterwards: the command prints the link.

## Health

### status

The state of the installation: version, the GitHub App and where it is installed, recent
webhook deliveries, the webhook URL, the label prefix jobs must use, the static egress IP,
running machines, the launch and dead-letter queues, today's jobs and spend, this month's
estimate, image builds and the dashboard link.

```bash
truner status
```

### doctor

Checks the installation end to end and explains each problem: credentials, the stack,
whether a newer release is out, whether the installation is locked against older CLIs,
the control plane, EC2 quotas (standard and GPU), existing subnets without public IPs,
the dead-letter queue, the account config, launch failures of the last 24 hours, the
GitHub App's permissions, events and installations, and recent webhook deliveries.
Under **Jobs** it prints the label prefix and lists queued jobs that no machine is coming
for: jobs whose labels carry another installation's prefix, and jobs whose machine ran
another job with the same labels and that got no machine again. GitHub confirms each one
is still queued.

```bash
truner doctor
```

It exits with code 0 when everything looks good, and otherwise with
`N problem(s) found`.

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

| Flag | Default | Meaning |
|---|---|---|
| `--by repo\|workflow\|type` | `repo` | group by repository, workflow or instance type |
| `--days N` | `30` | look back this many days |

### logs

A runner machine's log (bootstrap, agent steps and runner output), or the control plane's
logs.

```bash
truner logs 123456789                  # a job id or runner name
truner logs --control-plane            # webhook, worker and janitor
truner logs --control-plane -f         # follow
truner logs --control-plane --since 6h 123456789
```

| Flag | Default | Meaning |
|---|---|---|
| `--control-plane` | | show the webhook, worker and janitor logs. An argument then filters them |
| `-f`, `--follow` | | keep printing new control plane logs |
| `--since DURATION` | `1h` | how far back to read control plane logs |

Without an argument, `truner logs` shows the control plane logs. Machine logs are
uploaded to S3 when the machine ends. MicroVM image builds and MicroVMs log to the
CloudWatch log group `/truner/<installation>/microvms`, which `truner logs` does not read.

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

For a job id, it connects to the machine the job ran on. `--ssh` runs `ssh runner@<public
IP>`. Without the AWS CLI, it prints the `aws ssm start-session` command to run. Add
`keep=30m` to a job's labels to inspect its machine after the job finished. MicroVMs
have no shell access.

### pools

Each [warm pool](pools.md)'s ready, preparing and in-use machines.

```bash
truner pools
```

### images

Optimized runner images (AMIs). See [images](images.md).

```bash
truner images list
truner images build ubuntu24-full --arch all    # x64 (default), arm64 or all
truner images delete ami-0123456789abcdef0
```

`list` shows the baked AMIs, the [MicroVM](microvms.md) images with their state and any
build error, and recent builds. `build` starts a build unless one is already running,
also right after a failed one, and builds take 5–20 minutes. `delete` deregisters an AMI
and deletes its snapshots.

### quota

Shows the EC2 vCPU quotas (spot and on-demand, standard and GPU families) and requests
increases.

```bash
truner quota
truner quota --request 512           # spot and on-demand, standard families
truner quota --gpu --request 64      # spot and on-demand, G and VT families
```

## Cache

Inspect and clean the [magic cache](caching.md). All subcommands accept `--repo owner/name`.

```bash
truner cache ls --repo my-org/app
truner cache stats                   # size per repository
truner cache rm node-cache-          # entries whose key starts with this prefix
truner cache purge --repo my-org/app # all entries of the repository, or of all without --repo
```

`purge` asks for confirmation unless `--yes` is given. [Repository
snapshots](caching.md#repository-snapshots) are cache entries with the key
`truner-repository-snapshot`.

## Configuration

The account-wide [configuration](configuration.md).

```bash
truner config show                          # prints an example when there is none
truner config edit                          # opens $VISUAL or $EDITOR, validates, uploads
truner config apply truner.yml
truner config validate truner.yml
truner config validate --repo .github/truner.yml
```

A saved configuration applies to new jobs within a minute. `validate` checks a file
locally without AWS. A file in a `.github/` folder is checked as a repository config
even without `--repo`.

## GitHub

```bash
truner github status     # the app and its last 20 webhook deliveries
truner github install    # open the page to install the app on more repositories
```

`truner status` lists where the app is installed.

## Other

```bash
truner version
truner completion bash|zsh|fish|powershell
```

`truner version` prints the CLI's version and says when a newer release is out.
