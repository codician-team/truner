# Getting started

truner runs every GitHub Actions job on a fresh EC2 machine in your own AWS account.
This page takes you from nothing to a first job, which takes about five minutes.

## What you need

- An **AWS account**. The installer needs administrator access. Day-to-day commands
  need much less.
- A **GitHub organization or personal account** on github.com.
- A computer with a browser to create the GitHub App. The installer opens it for you.

You don't need Go, Node.js, Terraform or the AWS CLI. The `truner` binary contains
everything it deploys.

## 1. Install the CLI

::: code-group

```bash [macOS / Linux]
curl -fsSL https://raw.githubusercontent.com/truner/truner/main/install.sh | sh
```

```powershell [Windows]
irm https://raw.githubusercontent.com/truner/truner/main/install.ps1 | iex
```

:::

Or download a binary for your platform from the
[releases page](https://github.com/truner/truner/releases), and check it against
`checksums.txt`.

## 2. Install truner into your AWS account

```bash
truner install
```

The installer walks through five steps:

1. **AWS account.** It uses your current AWS profile or environment variables. If it
   finds no working credentials, it asks you to paste an access key and can save it as
   the AWS profile `truner`.
2. **Preflight checks.** It checks the region's availability zones and your EC2 vCPU
   quotas.
3. **Control plane.** It uploads the Lambda package and agent to an artifacts bucket and
   deploys a CloudFormation stack, which takes 2–4 minutes.
4. **GitHub App.** It opens your browser to create a private GitHub App with one click,
   then to install it on the repositories you choose. The app's credentials are stored
   encrypted in SSM Parameter Store.
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
    runs-on: truner/cpu=4/run=${{ github.run_id }}   # was: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-node@v4
        with: { node-version: 22, cache: npm }        # cached in your S3 bucket
      - run: npm ci && npm test
```

Push it, and watch the job get a machine:

```bash
truner jobs --watch
```

::: info Why `run=${{ github.run_id }}`?
GitHub gives a job to *any* online runner whose labels match. truner registers each
machine with exactly its job's labels, and the unique token keeps it from taking a job
of another workflow run. It is optional but recommended.
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
