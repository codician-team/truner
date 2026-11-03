# Field report: moving a CI onto MicroVMs (v0.1.3–v0.1.5)

On 2026-11-03 and 2026-11-04 the CI of one repository moved from EC2 jobs onto truner
MicroVMs, while the installation went from v0.1.3 to v0.1.5. This page lists what that
found, for the people who work on truner. Each finding has the numbers, the likely
mechanism in the code and a suggested improvement. The repository is described only by
its shape.

**The repository:** a 160 MB TypeScript monorepo (about 4,100 files, about 900 test
files, Node 24, vitest). Its CI runs typecheck, three test parts, an add-on check, an
Astro check and a deploy job. All times are from us-east-1.

| # | Finding | Suggested improvement |
|---|---|---|
| 1 | A downgrade made every micro job run on EC2 for 50 minutes, unnoticed | Fail, or flag loudly, jobs whose labels the control plane does not know |
| 2 | The first micro jobs after an upgrade run on EC2 | Rebuild MicroVM images (and AMIs) at the end of `truner upgrade` |
| 3 | Checkouts from a snapshot are slower than from a per-commit `.git` cache | Reuse a snapshot of the run's own commit; find out why some fetches take 12–18 s |
| 4 | Without a snapshot, every parallel job fetches everything and saves its own | One saver per repository and ref |
| 5 | Jobs see a MicroVM's peak size, not its baseline | Document it, and what bursting costs |
| 6 | Measured start and run times | Add them to the docs |
| 7 | A job held by a concurrency group starts about 70 s after its release | Document it; truner does not launch early |
| 8 | Node 26 does not start on a MicroVM | Add `libatomic1` to the MicroVM image and document it |
| 9 | Smaller notes | `truner jobs` WAIT, a fixed hint |

## 1. A silent downgrade, then a silent fallback to EC2

**What happened.** An old v0.1.1 CLI ran `truner upgrade` on the v0.1.3 installation. It
deployed v0.1.1 and reported success (this is the incident behind
[v0.1.5](https://github.com/codician-team/truner/blob/main/CHANGELOG.md#v015)). v0.1.1
does not know the `micro` label key. For about 50 minutes every job labelled
`micro/cpu=4` ran on an EC2 machine with the default size, passed, and looked normal in
GitHub. It was noticed only because the jobs took longer to start.

**Mechanism.** `Resolve` (`internal/config/resolve.go`) turns an unknown key into the
warning `ignored unknown label key "micro"` and resolves the rest of the label. The
agent prints spec warnings as a `::warning` annotation in the job (`internal/agent/flow.go`,
the job banner; v0.1.1 did the same), so the warning was there, but nobody reads the
annotations of a green run.

**Since then.** v0.1.5 updates the CLI before `upgrade` and locks stacks against older
CLIs, so this downgrade cannot happen again. Version skew can still happen the other
way: a workflow can use a label key that is newer than the installation, for example
after a docs page is published before an installation is upgraded.

**Suggestion.** A label key the control plane does not know should fail the job with
the reason and the command that fixes it (`truner upgrade`), the way `micro/arch=x64`
fails on `main`. A typo in a key then fails too, which is what a user
wants. If failing is too strict, an account setting such as `unknownLabels: warn` could
keep today's behavior. At the least, `truner jobs` and `truner doctor` could list jobs
that ran with spec warnings.

## 2. The first jobs after an upgrade run on EC2

**What happened.** After `truner upgrade` from v0.1.3 to v0.1.5, the MicroVM images
built by v0.1.3 could no longer be used. The first micro job started a 3–4 minute image
build and ran on an EC2 Graviton machine, as did every micro job of that size queued
during the build. One deploy job queued about a minute before the image was ready also
ran on EC2. The jobs passed, but each paid an EC2 start (43–67 s) instead of a MicroVM
start (7–14 s).

**Mechanism.** `MicroImage` (`internal/controlplane/micro.go`) treats an image as usable
only when it was built by the running control plane:
`usable := it.Str("imageVersion") != "" && it.Str("agent") == cp.Env.Version`. The only
caller is the worker, when it handles a job (`internal/controlplane/worker.go`), so a
rebuild starts with the first job after the upgrade. Baked AMIs work the same way:
`AutoBake` (`internal/controlplane/images.go`) rebakes images baked by another truner
version, but it runs once an hour from the janitor, and the worker bakes on demand.

**Suggestion.** At the end of `truner upgrade` (or in the first janitor run after the
control plane's version changes), start building the MicroVM images of every size that
was used in the last days, and bake the AMIs in use. The upgrade could wait for the
builds, or print that micro jobs run on EC2 for the next 3–4 minutes. A softer variant:
keep using the old image until the new one is ready, when the agent protocol between
the two versions is compatible. Then an upgrade never sends micro jobs to EC2.

## 3. Repository snapshots against a per-commit `.git` cache

**What happened.** The repository's snapshot was 152.5 MiB and was restored from S3 in
1.7–2.1 s. The fetch that followed, of the objects newer than the snapshot, took 1.5 s
in some jobs and 12–18 s in others. Over 14 checkouts the checkout step took 15.5 s at
the median.

The same workflow then tried a workflow-level `actions/cache` of `.git`, keyed by the
commit, which the magic cache also stores in S3:

```yaml
- uses: actions/cache@v6
  with:
    path: .git
    key: git-${{ github.sha }}
    restore-keys: git-
```

With it, the checkout step took 11 s at the median. The difference was largest for a
job that runs after another job of the same run, such as a deploy job: 19–22 s from the
snapshot against 5 s from the exact-commit cache, which leaves nothing to fetch. In one
case the snapshot's commit was not an ancestor of the checked-out commit, because the
branch had been force-pushed.

**Mechanism.** `save` (`internal/agent/repocache.go`) writes a snapshot only when the
job's ref has none, when the one it found is older than `repoSnapshotMaxAge` (6 hours),
or when a default-branch job restored another ref's snapshot. So a busy default branch
keeps a snapshot up to 6 hours old, every job fetches everything since then, and the
later jobs of a run fetch again what its first jobs already fetched.

Why the same fetch sometimes takes 1.5 s and sometimes 12–18 s is not known. 152.5 MiB in
12–18 s would fit GitHub sending most of the tree again, as if it ignored the restored
commit as a `have` (the snapshot's commit is shallow, and in one case not an ancestor),
or GitHub taking that long to compute a custom pack.

**Suggestions.**
- Let a run reuse its own commit: after a job whose fetch transferred more than a little
  (or took more than a few seconds), save a snapshot of that commit, at least for the
  rest of the run. Or refresh the default branch's snapshot much more often than every
  6 hours, for example when the snapshot is more than N commits or M MiB behind.
- Log the fetch that follows a restore: its time and the bytes received
  (`git fetch --progress` prints them, and the wrapper sees the fetch). That tells the
  two explanations above apart.
- Until then, the docs could mention the per-commit `actions/cache` of `.git` as an
  option for deploy-style jobs.

## 4. Cold snapshot saves are duplicated

**What happened.** In the first run without a snapshot, all 6 parallel jobs fetched the
whole repository from GitHub, which took 39–291 s (six full fetches at once from
MicroVMs). Each job then saved its own 152.5 MiB snapshot to the same key.

**Mechanism.** `save` saves whenever `found == ""`, and nothing coordinates the jobs of a
run. The last upload wins, so the result is correct, but six jobs spend the upload time
and the S3 requests.

**Suggestion.** One saver per repository and ref: take a short lock in the cache store
before the upload, or refuse the upload with `already_exists`, as the cache already does
for taken cache keys since v0.1.1. The 39–291 s fetches are GitHub's; a cold start could
be avoided by saving a snapshot at install time, or with a `truner cache warm <repo>`
command.

## 5. Jobs see a MicroVM's peak size

**What happened.** A `micro/cpu=4` MicroVM reports `nproc` 16 and 32 GiB of memory: its
peak, not its baseline. Test runners that size their worker pools from the CPU count
(vitest's default is one less than the CPU count) started 15 workers. That worked well: the
vCPUs are whole Graviton cores, and tests bounded by CPU time passed with headroom.

**Suggestion.** [MicroVMs](microvms.md#sizes) lists the peak sizes but does not say that
jobs see them. It could say that `nproc`, `os.cpus()` and `/proc/meminfo` report the peak,
that tools size themselves to it, and what a job pays for running at the peak (the price
per vCPU-second and GiB-second above the baseline), so that users can decide whether to
cap their worker pools.

## 6. Measured benefit

Numbers for the docs, from the same repository and the same day:

| | EC2 | MicroVM |
|---|---|---|
| Queued to first step | 43–67 s | 7–14 s |
| `actions/checkout` with the same cache | 4–13 s | about 13 s |

- **End to end**, moving every job to MicroVMs took a push from about 2m55s to about
  2m45s until it was live, and the checks from 116–156 s to 88–91 s.
- **Graviton against x64.** The heaviest file of one unit-test part ran about 20 %
  slower on Graviton than on x64 AMD.
- **One big machine against three MicroVMs.** The whole test suite on one 32 vCPU EC2
  machine took 46 s, plus a 43 s boot. Three MicroVM parts finished in 54–65 s.

These agree with [parallel jobs](parallel-jobs.md#start-times) (MicroVMs 6–20 s, EC2
43–90 s), and the checkout row shows that the checkout is now the largest fixed cost of
a MicroVM job (see finding 3).

## 7. Jobs held by a concurrency group

**What happened.** The deploy job has a job-level `concurrency` group. One deploy job
was created while the previous run's deploy held the group, and started about 70 s after
the group was released. `truner jobs` showed 1m25s of WAIT for it.

**Checked in the control plane logs.** truner does not launch a machine for a job that
GitHub holds:

| Time | Event |
|---|---|
| 0 s | GitHub creates the job; the group is held |
| +15 s | the previous deploy completes and releases the group |
| +79 s | the webhook receives `queued` |
| +81 s | the worker launches a MicroVM |
| +85 s | the job starts |

GitHub sends `queued` only when it releases the job, and the control plane launches
only on `queued` (`internal/controlplane/webhook.go`), so there is no idle, billed
machine. Of the 70 s after the release, 64 s passed before GitHub sent `queued`, and
truner took 6 s. This is one sample; GitHub may not always take that long.

**Suggestions.**
- Document how such jobs behave: no machine until GitHub queues the job, and GitHub
  may queue it a minute after the release.
- `truner jobs` WAIT counts from the job's `created_at` (`onQueued` in
  `internal/controlplane/webhook.go`), so it includes the time GitHub held the job. The
  control plane knows when `queued` arrived; showing the time from `queued` to the
  start apart from WAIT would separate truner's share from GitHub's.

## 8. Node 26 does not start on a MicroVM

**What happened.** On 2026-11-03 the repository moved from Node 24 to Node 26, because
Node 24 has a V8 Maglev deoptimization bug that is fixed only in Node 26.12 and later.
On MicroVMs, the `node` binary then exited with status 127 before `actions/setup-node`
could run it: Node's official Arm64 Linux build links `libatomic.so.1`, and the MicroVM
image does not have it. The workaround is a setup step that installs the library when
it is missing:

```yaml
- run: |
    if ! ldconfig -p | grep -q libatomic.so.1; then
      apt-get update && apt-get install -y libatomic1
    fi
```

It costs each MicroVM job about 10 s, most of it `apt-get update`. EC2 Graviton machines
were not checked.

**Mechanism.** `microDockerfile` (`internal/controlplane/micro.go`) installs a fixed
package list with `--no-install-recommends`, and `libatomic1` is not in it. The base
image is `public.ecr.aws/ubuntu/ubuntu:24.04`, which does not have it either.

**Suggestion.** Add `libatomic1` to the MicroVM image's package list, check the Arm64
AMI (the `standard` profile) for it too, and list it under
[What a MicroVM has](microvms.md#what-a-microvm-has). Node is the most common runtime
on these machines, so other common runtime libraries may be worth a look at the same
time.

## 9. Smaller notes

- `truner jobs` WAIT matches GitHub's `created_at` to `started_at` for the job.
- The v0.1.1 CLI's "No jobs yet" hint printed the wrong label prefix. Fixed in v0.1.4.
