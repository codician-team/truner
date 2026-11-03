# FAQ

## General

### How is this different from GitHub-hosted runners?

Jobs still get a clean machine each time, but the machine runs in your AWS account. You
pay EC2 prices, usually spot, and can pick any instance shape: Arm64, GPUs, local NVMe,
96 vCPUs. Machines run inside your VPC with your IAM role, and the cache lives in your S3
bucket with no size limit.

### How is it different from running my own self-hosted runners?

Long-lived self-hosted runners keep machines running around the clock, carry state from
one job to the next, and have to be patched and scaled. With truner, each job gets a new
machine that disappears afterwards. The control plane is serverless, so nothing runs
while no jobs are queued.

### Is there a hosted service, license key or telemetry?

No. Everything runs in your AWS account and talks only to GitHub and AWS.

### Which GitHub plans work?

Any github.com plan (Free, Team or Enterprise Cloud), for organizations and personal
accounts. Check GitHub's current pricing page for how self-hosted runner usage is billed
on your plan.

### GitHub Enterprise Server?

Not supported yet.

### macOS runners?

No. EC2 Mac instances require dedicated hosts with a 24-hour minimum allocation, which
does not fit a machine-per-job model.

## Running jobs

### How fast does a job start?

Most of the time goes to EC2 booting the machine. Typically:

- **Baked image:** about 25–45 seconds from queued to running.
- **Warm pool:** about 10–20 seconds.
- **MicroVM (beta, `micro` label):** about 6–10 seconds, since the MicroVM resumes from
  a snapshot with the runner in memory instead of booting. For short jobs without Docker;
  see [MicroVMs](microvms.md).
- **First use of an image:** longer, because the software is installed at boot while the
  optimized image is being built.
- **Windows:** 3–5 minutes.

The first line of every job log shows how long the machine took to become ready.

### What happens on a spot interruption?

If it happens before the job starts, truner launches an on-demand machine instead. If it
happens during the job, GitHub marks the job as failed and truner re-runs it on
on-demand capacity once the workflow run finishes. You can turn that off with
`retryOnInterruption: false` in the account config.

### Do `container:` jobs and service containers work?

Yes. Docker is preinstalled, and the magic cache works in container jobs too.

### Can I use the same tools as on `ubuntu-latest`?

The default `ubuntu24` image has a standard toolset: Docker, git, the GitHub CLI, the AWS
CLI, Python and build tools. `setup-*` actions install language versions as usual.
`image=ubuntu24-full` adds Node.js, Go, Rust, Java, .NET and much more. For an exact
match, build an AMI from [actions/runner-images](https://github.com/actions/runner-images)
and use it as a [custom image](images.md#custom-images).

### Can jobs use AWS without stored secrets?

Yes. Attach policies to the runner role with
`truner install --runner-policy arn:aws:iam::aws:policy/...`. Every job of the
installation gets those permissions. For production deployments, prefer GitHub OIDC with
a role scoped to the repository and branch.

### Why does a job wait in the queue?

Run `truner doctor`. The usual causes are a label with another installation's prefix
(`truner/…` where the installation answers to `truner-acme/…`), an exhausted EC2 vCPU
quota (`truner quota --request 512`), a repository the GitHub App is not installed on,
or a reached budget or instance limit. See [troubleshooting](troubleshooting.md).

### Why does one job of a run wait while the others run?

Most likely its machine ran another job with the same labels. truner launches a machine
again for it, so it starts later than the others. Give every job its own run token,
`run=${{ github.run_id }}-<job>`, with the matrix value in matrix jobs. See
[one run token per job](labels.md#one-run-token-per-job).

### Should I split one long CI job into several?

Usually yes, if the checks are independent: they then run at the same time. Each job
pays for its own start (seconds on a MicroVM, about a minute on EC2) and its own
checkout. [Splitting CI into parallel jobs](parallel-jobs.md) has the numbers and the
pitfalls.

## Security

### Is it safe for public repositories?

Each job runs on a fresh VM with a single-use runner registration that only works for its
own repository. All repositories in one installation share its cache bucket and machine
role, though. Use separate installations (`--stack`) for different trust levels, and do
not run pull requests from forks on self-hosted runners. See [security](security.md).

### What permissions does the GitHub App have?

`administration: write` (GitHub requires it to register repository runners),
`actions: write` (to read jobs and re-run interrupted ones), `contents: read` (for
`.github/truner.yml`) and `metadata: read`. Install it only on the repositories that need
runners.

## Operating

### How do I upgrade?

Run `truner upgrade`. It updates the CLI to the latest release first, then the
installation. Machines that are running finish their jobs, and new jobs use the new
version. With a CLI older than v0.1.5, install the latest CLI first
(see [getting started](getting-started.md)).

### How do I remove it?

`truner uninstall` terminates every machine and deletes the baked images, the data
bucket and the stack. After that, delete the GitHub App in your GitHub settings, because
GitHub has no API for removing apps.
