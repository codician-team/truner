# MicroVMs (beta)

Short jobs such as linters, gates and checks spend most of their time waiting for a
machine to boot. A **micro** job skips the boot: it runs on an
[AWS Lambda MicroVM](https://aws.amazon.com/lambda/lambda-microvms/) that resumes from a
snapshot taken with the runner already installed and in memory. Heavier jobs keep
running on EC2.

```yaml
jobs:
  lint:
    runs-on: truner/micro/run=${{ github.run_id }}-lint     # 2 vCPU MicroVM
    steps:
      - uses: actions/checkout@v7
      - run: ./scripts/lint.sh
  test:
    runs-on: truner/cpu=8/run=${{ github.run_id }}-test     # EC2, as before
```

::: warning Beta
MicroVM support is new and has not yet been run at scale. When a MicroVM cannot run a
job, the job falls back to EC2 (see below), so a micro label never leaves a job queued.
:::

## Sizes

`cpu` picks the MicroVM's baseline. Memory is 2 GiB per vCPU, and both burst to four
times the baseline while the job needs it.

| Label | Baseline | Peak | Disk |
|---|---|---|---|
| `micro/cpu=1` | 1 vCPU, 2 GiB | 4 vCPU, 8 GiB | 8 GiB |
| `micro` (or `micro/cpu=2`) | 2 vCPU, 4 GiB | 8 vCPU, 16 GiB | 16 GiB |
| `micro/cpu=4` | 4 vCPU, 8 GiB | 16 vCPU, 32 GiB | 32 GiB |

`ram=N` picks the smallest size whose peak memory covers it. Larger jobs fail with a
message asking you to remove `micro`.

The label also works in runner profiles:

```yaml
# .github/truner.yml
runners:
  check:
    micro: true
    cpu: 1
```

```yaml
runs-on: truner/runner=check/run=${{ github.run_id }}-check
```

## What a MicroVM has

- **Ubuntu 24.04 userland on Arm64 (Graviton)**, with git, git-lfs, curl, wget, jq,
  Python 3 with pip and venv, make, zip, xz, zstd, rsync, an SSH client, and
  `libatomic1`, which Node.js 26 and later need. Jobs run as
  root, because Lambda's MicroVM container does not let `sudo` raise a user's
  privileges; `sudo` still works, and `apt-get install` works for anything else. `setup-node`, `setup-python`,
  `setup-go` and similar actions download Arm64 builds as usual.
- **The magic cache**, so `actions/cache` and `setup-*` caching use your S3 bucket as on
  EC2.
- **AWS access** through its own role, with the same cache permissions as EC2 runners
  and the policies you added with `--runner-policy`.

What it does not have:

- **No Docker.** `container:` jobs, service containers and `docker build` need EC2.
- **No x64.** A job that asks for x64 together with `micro`, through `arch=x64`, its
  runner profile or a runner name ending in `-x64`, fails with an explanation. An
  `arch: x64` in the account or repository `defaults` is overridden, and the job runs on
  Arm64.
- **No Windows, GPUs, local NVMe, SSH access, private subnets, warm pools or instance
  families and types.** These labels together with `micro` fail the job with an
  explanation.
- **Its own image.** `image` is ignored with a warning in the job log.
- **At most 8 hours.** `timeout` is capped at 480 minutes, and `keep` is ignored with a
  warning.

## How it works

1. The first micro job of each size makes truner build a **MicroVM image** for it. Lambda
   builds it from a short Dockerfile (Ubuntu plus the tools above) and starts the truner
   agent, which downloads the runner and reads its files into memory. Lambda then
   snapshots the machine. The build takes about 3–4 minutes, and **that first job runs
   on EC2** in the meantime.
2. Later micro jobs get a MicroVM resumed from the snapshot. Its job's runner
   configuration arrives in the MicroVM's start hook, and the runner starts right away.
   The job log's first line shows the MicroVM and how long it took to become ready. In
   our first tests in us-east-1, the runner was ready 1–3 seconds after the MicroVM
   started, and jobs were running 6–9 seconds after GitHub queued them. On EC2 the same
   installation needs 44–66 seconds.
3. When GitHub reports the job done, the control plane terminates the MicroVM. The
   janitor ends any MicroVM whose job is over or that no job knows about.
4. After `truner upgrade` and after each new `actions/runner` release, the images are
   rebuilt. Jobs use EC2 while an upgrade rebuild runs, and the previous runner version
   while a runner update builds. Replaced image versions are deleted after 9 hours.

**A micro job falls back to an EC2 Graviton machine with the same vCPUs** when its image
is still building or its build failed, when the installation has no MicroVM roles yet,
when the region has no Lambda MicroVMs, when the account's MicroVM quota is used up, or
when a MicroVM started but did not take the job within 5 minutes. The job log explains
why with a warning. That machine has 4 GiB per vCPU, the default `ubuntu24` image and a
40 GiB disk, and it uses spot with on-demand fallback. After a failed build, truner
tries again within an hour, or with the next job after `truner upgrade`.

`truner images list` shows the MicroVM images, their state and any build error. Build and
MicroVM logs are in the CloudWatch log group `/truner/<installation>/microvms`.
`truner logs` does not read that group: open it in the CloudWatch console, or use
`aws logs tail /truner/<installation>/microvms --follow`.

## Costs

MicroVMs cost more per second than EC2, and truner uses them to save start time, not
money. Lambda bills a running MicroVM's baseline per second, plus the snapshot data it
reads when it starts. In us-east-1:

| Size | Per hour at baseline | Snapshot reads per start (up to) |
|---|---|---|
| 1 vCPU, 2 GiB | $0.126 | $0.003 |
| 2 vCPU, 4 GiB | $0.252 | $0.006 |
| 4 vCPU, 8 GiB | $0.504 | $0.012 |

Bursting above the baseline is billed for the extra vCPU and memory while used. For
comparison, a 2 vCPU Graviton EC2 machine costs about $0.07 per hour on demand and
usually less on spot, but it bills at least 60 seconds plus the boot. For a one-minute
check, a MicroVM costs about a cent and an EC2 spot machine a fraction of that. `truner
explain truner/micro` prints the prices, and `truner costs` includes micro jobs with
these estimates.

## Requirements

- A region where [Lambda MicroVMs](https://docs.aws.amazon.com/lambda/latest/dg/lambda-microvms-guide.html)
  are available. Elsewhere, micro jobs always run on EC2.
- The MicroVM roles, which installations get from a truner release with MicroVM support
  (v0.1.3 or newer). Run `truner upgrade` with such a CLI on older installations.
- A CLI of v0.1.3 or newer. Older ones do not know the `micro` label in `truner explain`
  and do not list MicroVM images in `truner images list`, even when the installation
  runs micro jobs.
- Enough MicroVM quota. Lambda limits the memory of all running MicroVMs per region;
  raise it in Service Quotas under **Lambda MicroVMs**. `limits.maxInstances` counts
  MicroVMs together with EC2 machines.
