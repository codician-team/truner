# Splitting CI into parallel jobs

One CI job runs its checks one after another. Split into jobs, the checks run at the
same time on machines of their own, and a run takes as long as its slowest job plus
what each job costs before its first step. This page is what we measured when
[codician-team/bake3d](https://github.com/codician-team/bake3d) moved its CI (a
TypeScript monorepo of 237 MB with typecheck, unit tests, `astro check`, a build and a
deploy) from one job onto parallel truner jobs on 2026-11-03.

## A layout that works

```yaml
jobs:
  typecheck:
    runs-on: truner/micro/cpu=4/run=${{ github.run_id }}-typecheck
  astro-check:
    runs-on: truner/micro/cpu=4/run=${{ github.run_id }}-astro-check
  build:
    runs-on: truner/micro/cpu=4/run=${{ github.run_id }}-build
  test:
    strategy:
      fail-fast: false           # every shard reports its failures
      matrix:
        shard: [1, 2, 3]
    runs-on: truner/cpu=8/ram=32/family=m7a+m8a+r7a+r8a/run=${{ github.run_id }}-test-${{ matrix.shard }}
  add-ons:                       # runs a native Linux x86_64 binary
    runs-on: truner/cpu=4/run=${{ github.run_id }}-add-ons
  deploy:
    needs: [typecheck, astro-check, build, test, add-ons]
    runs-on: truner/micro/cpu=4/run=${{ github.run_id }}-deploy
```

- **Short checks on MicroVMs** (`micro`): they need no Docker and start within seconds.
  MicroVMs are Arm64, and so is the EC2 machine a micro job falls back to.
- **x64 EC2 for what needs it:** native x86_64 binaries, Docker, and tests whose time
  bounds were set on x64.
- **Whole-core families for the timed tests** (see [below](#timing-sensitive-tests)).
- **A run token per job** (see [below](#one-run-token-per-job)).

With this layout bake3d's checks take 3m52s, and a push is live about 4½ minutes after
it lands, deploy and smoke test included.

## Start times

Measured from the time GitHub queued the job to the time its first step ran, in
us-east-1 on truner v0.1.3:

| Machine | Queued to running | Example from the job banner |
|---|---|---|
| MicroVM | 6–20 s | `ready 0.7s after start, 7s after the job was queued` |
| EC2, baked image | 43–90 s | `r7a.2xlarge spot … ready 46s after boot, 65s after the job was queued` |

The first micro job of each size after `truner upgrade` builds that size's MicroVM image
(3–4 minutes) and runs on an EC2 Graviton machine meanwhile, as do the jobs queued
during the build. See [MicroVMs](microvms.md#how-it-works).

## What a job costs before its first step

Every job starts on a new machine, so each one checks out the repository, sets up its
tools and installs its dependencies again. Most of that is cheap on truner. The checkout
is not.

| Step | EC2 | MicroVM |
|---|---|---|
| `actions/checkout` of 237 MB, `fetch-depth: 1` | 11–102 s, 95 s of `git fetch` in one job | about 17–18 s |
| `setup-node` restoring a 234 MB npm cache from the magic cache | about 1 s (download at 289–337 MiB/s) | about 1 s |

In one run (41669209801), the same checkout took 29 s on an `r7a.2xlarge` (27.5 s of
`git fetch`), 11 s on an `i4i.xlarge` and 18 s on a MicroVM. The fetch goes to GitHub, so
the magic cache does not help it, and its time on EC2 varied the most of any step.
Caching dependencies, on the other hand, is nearly free: the job-completed hook
reported `restored 233.7 MiB at 313.3 MiB/s`.

To keep the checkout short:

- Keep the default `fetch-depth: 1`.
- Check out only what a job needs: `sparse-checkout:` or `filter: blob:none` on
  `actions/checkout`.
- Keep large binary files out of the repository, or in Git LFS and fetched only by the
  jobs that use them.
- Count the checkout when you decide how far to split: a job that runs for 20 seconds
  after a 30-second checkout is better merged into another one.

Caching the repository's git objects in the magic cache is being measured
([#4](https://github.com/codician-team/truner/issues/4)).

## Hidden ordering dependencies

A single job's steps can depend on each other without saying so, and splitting the job
breaks them. In bake3d, typecheck generated a gitignored file (`wrangler types` writes
`worker-configuration.d.ts`) and `astro check` read it later in the same job. On a
machine of its own, `astro check` found no types and failed. Look for steps that read
generated or gitignored files, environment from `$GITHUB_ENV`, or tools that an earlier
step installed, and have each job make what it reads.

## Handing build output to another job

An artifact is the obvious way to pass a build to the deploy job, but artifacts count
against GitHub's storage for Actions: 500 MB on the Free plan. bake3d's build was about
106 MB zipped, so a few runs would fill it. The alternatives:

- **Build again in the job that needs it.** For bake3d, building took about as long as
  downloading the artifact would have, and the deploy job now checks the production
  build on every run.
- **Pass it through your S3 bucket.** The magic cache stores `actions/cache` entries in
  the installation's bucket with no size limit (`key: build-${{ github.sha }}` in both
  jobs), or copy files to `$TRUNER_S3_BUCKET` with the AWS CLI and the machine's role.

## One run token per job

Jobs with equal labels can take each other's machines: matrix shards, or several
`micro/cpu=4` checks. That is harmless while every job gets a machine. When GitHub
delivers a `queued` event late, after its job has started on another job's machine, one
job is left without a machine. It happened in one of bake3d's first parallel runs: the
`build` job waited until the run was cancelled. A token per job,
`run=${{ github.run_id }}-<job>`, gives every job a machine of its own. See
[labels](labels.md#one-run-token-per-job).

## Timing-sensitive tests

Without `family`, EC2 Fleet picks a spot pool by price and capacity among all matching
types, and the pick changes from run to run. On Intel and some AMD families a vCPU is a
hyper-thread. bake3d's test shard landed on a `z1d.2xlarge`: 8 vCPUs on 4 cores under 7
vitest workers. A BVH test that allows 8 times the CPU time of a cache-resident
reference workload read 8.9 there and failed. On `r7a` and `m7a`, where every vCPU is a
whole core, it reads about 3. Pin whole-core families for such suites:
`family=m7a+m8a+r7a+r8a`, or Graviton with `arch=arm64`. See
[labels](labels.md#machine).

## Checklist

- Labels start with the installation's prefix (`truner status` prints it).
- Every job has its own run token, matrix jobs included.
- Short jobs without Docker or x64 binaries use `micro`.
- Timed tests run on pinned whole-core families.
- `setup-*` caching or `actions/cache` restores dependencies (the magic cache stores
  them in S3).
- The checkout is part of every job's cost.
- Each job makes the generated files it reads.
- Large build output is rebuilt or passed through S3 instead of artifacts.
