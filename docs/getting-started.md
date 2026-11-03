# Getting started

truner runs every GitHub Actions job on a fresh EC2 machine in your own AWS account.
This page takes you from nothing to a first job, which takes about five minutes.

::: tip Let an AI agent do it
Claude Code, Codex, Cursor and other coding agents can run all of these steps for you.
Copy the prompt from [Install with an AI agent](ai-agents.md).
:::

## What you need

- An **AWS account**. The installer needs administrator access. Day-to-day commands
  need much less.
- A **GitHub organization or personal account** on github.com.
- A browser on any device, for two clicks on GitHub.

You don't need Go, Node.js, Terraform or the AWS CLI. The `truner` binary contains
everything it deploys.

## 1. Install the CLI

::: code-group

```bash [macOS / Linux]
curl -fsSL https://raw.githubusercontent.com/codician-team/truner/main/install.sh | sh
```

```powershell [Windows]
irm https://raw.githubusercontent.com/codician-team/truner/main/install.ps1 | iex
```

:::

Or download a binary for your platform from the
[releases page](https://github.com/codician-team/truner/releases), and check it against
`checksums.txt`.

## 2. Install truner into your AWS account

```bash
truner install
```

The installer walks through five steps:

1. **AWS account.** It uses your current AWS profile or environment variables. If it
   finds no working credentials, it asks you to paste an access key and can save it as
   the AWS profile `truner`. (`truner login` does only this step.)
2. **Preflight checks.** It checks the region's availability zones and your EC2 vCPU
   quotas.
3. **Control plane.** It uploads the Lambda package and agent to an artifacts bucket and
   deploys a CloudFormation stack, which takes 2–4 minutes.
4. **GitHub App.** It prints a link (and opens it in your browser) where you create a
   private GitHub App with one click and then install it on the repositories you choose.
   The page is served by your new control plane, so the link works on any device. The
   app's credentials go straight into SSM Parameter Store in your account, encrypted.
   If the installer stops while it waits, run it again: it continues with the same link.
5. **Finishing.** It checks the control plane's health and starts building optimized
   Ubuntu images in the background. Jobs work right away and start faster once the images
   are ready.

Common options:

```bash
truner install --region eu-west-1             # pick the region
truner install --github-owner my-org          # create the app for an organization
truner install --alert-email ops@example.com  # budget and failure alerts
truner install --nat                          # static egress IP (~$32/month)
```

Run `truner install --help` for every option, or see the [CLI reference](cli.md#install).
Running `install` again on an existing installation upgrades it and applies changed
options.

::: tip New AWS accounts
New accounts often start with an EC2 quota of only a few vCPUs, which is enough for
one or two jobs at a time. Request more right away, because approval can take a while:

```bash
truner quota --request 512
```
:::

## 3. Run a job on truner

Change `runs-on` in any workflow of a repository the app is installed on:

```yaml
jobs:
  test:
    runs-on: truner/cpu=4/run=${{ github.run_id }}-test   # was: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: actions/setup-node@v7
        with: { node-version: 22, cache: npm }        # cached in your S3 bucket
      - run: npm ci && npm test
```

Push it, and watch the job get a machine:

```bash
truner jobs --watch
```

::: info Why `run=${{ github.run_id }}-test`?
GitHub gives a job to *any* online runner whose labels match. truner registers each
machine with exactly its job's labels. The run id keeps a machine from taking a job of
another workflow run, and the job's name after it (`-test`) keeps it from taking another
job of the same run. Give every job its own token: jobs with equal labels can take each
other's machines, and one of them may then wait for a second launch. See
[one run token per job](labels.md#one-run-token-per-job).
:::

::: tip Installed with `--label-prefix`?
Labels start with the installation's prefix instead of `truner`, for example
`truner-acme/cpu=4/…`. `truner status` prints it. A job with another prefix stays queued
with no machine.
:::

The job log starts with a summary of the machine: instance type, spot or on-demand,
availability zone and boot timings. It ends with the cache statistics.

## 4. Check the installation

```bash
truner doctor
```

`doctor` checks credentials, the stack, the control plane, EC2 quotas, the GitHub App
and its recent webhook deliveries, and recent launch failures. It prints a fix for every
problem it finds.

## Next steps

- Pick machine shapes with [labels](labels.md): CPUs, memory, Arm64, GPUs, Windows, NVMe.
- Define reusable profiles in `.github/truner.yml` with [configuration](configuration.md).
- Learn how the [magic cache](caching.md) and Docker layer caching work.
- Start jobs in seconds with [warm pools](pools.md).
- Set a [budget and limits](costs.md), and check `truner costs` after a few days.

## Upgrading and removing

```bash
truner upgrade      # update the installation to this CLI's version
truner uninstall    # remove everything truner created
```

Install a newer CLI first, then run `truner upgrade`. `uninstall` terminates every
machine and deletes the baked images, the data bucket and the stack. After that, delete
the GitHub App in your GitHub settings, because GitHub has no API for removing apps.
