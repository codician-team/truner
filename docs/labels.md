# Labels reference

A job runs on truner when one of its `runs-on` labels is `truner` (or starts with
`truner/` or `truner=`). Settings are `key=value` pairs, written as a path or a list:

```yaml
runs-on: truner/cpu=8/ram=32/run=${{ github.run_id }}-test
runs-on: truner=${{ github.run_id }}-test/cpu=8/ram=32       # first value is the run token
runs-on: [truner, cpu=8, ram=32, "run=${{ github.run_id }}-test"]
```

Unknown keys are ignored with a warning in the job log. Invalid values fail the job
with an explanation.

::: warning Use your installation's prefix
`truner` is the default prefix. An installation made with `--label-prefix` (or one of
several in an organization) answers only to its own prefix, for example
`truner-acme/cpu=8/…`. A job with another prefix waits in GitHub's queue with no machine
and no error. `truner status` prints the prefix, `truner doctor` lists jobs that wait
because of it, and `truner explain` warns when a label's prefix is not the
installation's.
:::

## One run token per job

GitHub gives a job to any online runner whose labels include all of the job's labels.
truner registers each machine with exactly the labels of the job it was started for, so
two jobs with the same labels can take each other's machines. The `run=` token makes
labels unique. Give every job its own:

```yaml
jobs:
  typecheck:
    runs-on: truner/micro/cpu=4/run=${{ github.run_id }}-typecheck
  build:
    runs-on: truner/micro/cpu=4/run=${{ github.run_id }}-build
  test:
    strategy:
      matrix:
        shard: [1, 2, 3]
    runs-on: truner/cpu=8/run=${{ github.run_id }}-test-${{ matrix.shard }}
```

- `${{ github.run_id }}` keeps machines from taking jobs of other workflow runs.
- The job's name after it keeps machines from taking other jobs of the same run. Write
  the name out: `github.job` is empty in `runs-on`. In a matrix, add the matrix value
  or `${{ strategy.job-index }}`.

With one token per run (`run=${{ github.run_id }}` alone), jobs with otherwise equal
labels, such as matrix shards or several `micro/cpu=4` checks, swap machines. That is
harmless while every job gets a machine. But GitHub sometimes delivers a job's `queued`
event late, after the job has already started on a machine launched for another job.
truner then launches nothing for the late job, and the job whose machine was taken waits
with no machine until it is cancelled or GitHub gives up a day later. In one run of three
`micro/cpu=4` jobs, astro check's `queued` event arrived 38 seconds after astro check had
started on typecheck's MicroVM. Typecheck had taken build's, and build waited until the
run was cancelled ([#3](https://github.com/codician-team/truner/issues/3)).
`truner doctor` reports such jobs.

## Machine

| Key | Values | Default | Notes |
|---|---|---|---|
| `runner` | profile name | – | Built-in or from config. Labels after it override it. Aliases: `profile` |
| `cpu` | `4`, `4-16`, `8+` | `2` | vCPUs: exact, inclusive range, or minimum |
| `ram` | `16`, `16-64` | `cpu × 4` | GiB. A single number is a minimum. Aliases: `memory`, `mem` |
| `family` | `c7a+m7a`, `c8g`, `m7i.*` | any current generation | Instance families. With a family, `ram` defaults to "any" |
| `type` | `c7i.4xlarge+c6i.4xlarge` | – | Exact types (skips attribute-based selection) |
| `arch` | `x64`, `arm64` | `x64` | `arm64` selects Graviton |
| `gpu` | `1`, `4`, `true` | `0` | NVIDIA GPUs (use a GPU image, e.g. `runner=gpu-nvidia`) |
| `nvme` | `true` | `false` | Require local NVMe SSD instance storage. SSD instance storage is used automatically whenever present |
| `spot` | `true`, `false`, `only` | `true` | `true`: spot with on-demand fallback. `only`: never on-demand |
| `micro` | `true` | `false` | Run on a Lambda MicroVM (1, 2 or 4 vCPU, Arm64, no Docker) instead of EC2. See [MicroVMs](microvms.md). Alias: `microvm` |

Without `family` or `type`, truner asks EC2 for **all current-generation instance
types** matching `cpu`/`ram`/`arch`, excluding burstable (t-family), bare metal and
accelerated types, and lets EC2 Fleet choose the best spot pool across all
availability zones (`price-capacity-optimized`). Families whose instance storage is
hard disks (`d2`, `d3`, `d3en`, `h1`) are left out too: they are cheap on spot, but
their disks take minutes to set up and build slower than EBS. Name the family to use
one anyway.

**Timing-sensitive tests.** The cheapest spot pool changes from run to run, and a vCPU
is not the same everywhere. On AMD's 7th and 8th generation (`c7a`, `m7a`, `r7a`, `c8a`,
`m8a`, `r8a`) and on Graviton, each vCPU is a whole core. On Intel types and older or
special families (`m6a`, `z1d`, `i4i`, `m7i`, …) a vCPU is one of two hyper-threads, so
an 8-vCPU machine has 4 cores. Tests with CPU-time or wall-clock bounds can pass on one
and fail on the next: a test bounded at 8× a reference workload read 8.9× on a
`z1d.2xlarge` (4 cores under 7 test workers) and about 3× on `r7a`/`m7a`. Pin families
for such suites, for example `family=m7a+m8a+r7a+r8a`, and name several so EC2 Fleet
still has spot pools to choose from. See
[Splitting CI into parallel jobs](parallel-jobs.md).

## Image and disk

| Key | Values | Default | Notes |
|---|---|---|---|
| `image` | `ubuntu24`, `ubuntu22`, `ubuntu24-full`, `ubuntu24-gpu`, `windows2025`, `windows2022`, custom name, `ami-…` | `ubuntu24` | Append `-arm64`/`-x64` to pick the architecture. See [images](images.md) |
| `disk` | GiB | `40` (Windows `50`+) | gp3 root volume |
| `disk-iops` | 3000–80000 | 3000 | gp3 provisioned IOPS |
| `disk-throughput` | 125–2000 | 125 | gp3 MiB/s |
| `disk-init` | 100–300 | – | EBS provisioned volume initialization rate (MiB/s), speeds up first reads of a fresh volume |

## Behavior

| Key | Values | Default | Notes |
|---|---|---|---|
| `cache` | `s3`, `github`, `off` | `s3` | Magic cache backend. See [caching](caching.md) |
| `private` | `true` | `false` | Use private subnets (static egress IP with `--nat`) |
| `ssh` | `true` | `false` | Install the triggering user's GitHub SSH keys (needs `--ssh-cidr`) |
| `keep` | `30`, `30m`, `2h`, `true` | `0` | Keep the machine after the job for debugging (`true` = 30 min) |
| `timeout` | `90`, `3h` | `720` (12 h) | Hard limit on the machine's lifetime |
| `idle-timeout` | minutes | `15` | Shut down if no job is picked up in this time |
| `pool` | pool name | – | Start a pre-warmed machine. Hardware labels are ignored, since the pool defines them |
| `run` | anything | – | Uniqueness token, ignored. One per job: `run=${{ github.run_id }}-<job>` ([why](#one-run-token-per-job)) |

## Built-in runner profiles

| Profile | Machine |
|---|---|
| `2cpu-linux-x64` … `96cpu-linux-x64` | N vCPU, ≥ 4 GiB per vCPU, x64 (2, 4, 8, 16, 32, 48, 64, 96) |
| `2cpu-linux-arm64` … `96cpu-linux-arm64` | the same on Graviton |
| `2cpu-windows-x64` … `64cpu-windows-x64` | Windows Server 2025, 80 GiB disk (2, 4, 8, 16, 32, 64) |
| `gpu-nvidia` | ≥ 4 vCPU, 16 GiB, 1 NVIDIA GPU, `ubuntu24-gpu`, 120 GiB disk |
| `gpu-nvidia-large` | ≥ 16 vCPU, 64 GiB, 1 NVIDIA GPU, 200 GiB disk |

## Precedence

```
built-in defaults < account config `defaults` < repo `.github/truner.yml` `defaults`
                  < runner profile (`runner=`) < other labels
```

`truner explain <labels>` prints the resolved spec and matching instance types with
current spot prices.

## Environment on the machine

Jobs see these variables, in addition to GitHub's:

| Variable | Example |
|---|---|
| `TRUNER` | `true` |
| `TRUNER_INSTANCE_TYPE`, `TRUNER_INSTANCE_ID`, `TRUNER_INSTANCE_LIFECYCLE`, `TRUNER_AZ` | `c7a.xlarge`, `i-0abc…`, `spot`, `us-east-1b` |
| `TRUNER_S3_BUCKET`, `TRUNER_S3_DOCKER_PREFIX` | for `buildx type=s3` caches |
| `TRUNER_ECR_CACHE` | ECR repository for registry caches |
| `TRUNER_NVME` | mount point of local NVMe (if any) |
| `ImageOS`, `RUNNER_TOOL_CACHE`, `AGENT_TOOLSDIRECTORY` | as on GitHub-hosted runners |
| `AWS_REGION`, `AWS_DEFAULT_REGION` | the installation's region |
