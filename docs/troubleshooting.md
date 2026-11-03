# Troubleshooting

Start with:

```bash
truner doctor
```

It checks credentials, the stack and whether it is locked against older CLIs, the control
plane, EC2 quotas, the dead-letter queue, the account config, the GitHub App
(permissions, events, installations), recent webhook deliveries, recent launch
failures and queued jobs that no machine is coming for, and prints the fix for each
problem.

## A job stays queued

GitHub's job page only says that the job is waiting for a runner. `truner doctor` lists
the queued jobs that no machine is coming for under **Jobs**: the first two causes
below.

1. **Label prefix.** The first label must be the installation's prefix: `truner`, or the
   `--label-prefix` it was installed with. `truner status` prints it, for example
   `truner-acme/cpu=4/…`. A job written for another prefix, such as `truner/cpu=8/…` in
   an organization whose installation is `truner-acme`, gets no machine, and `truner
   jobs` does not list it. The control plane logs `job labels use another label prefix`,
   `truner doctor` names the job and the label to use, and `truner explain <label>` warns
   when a label's prefix is not the installation's.
2. **The other jobs of the run started, this one did not.** Jobs with equal labels can
   take each other's machines. If GitHub delivers a job's `queued` event late, after that
   job has started on a machine launched for another job, one job with those labels is
   left without a machine. The control plane logs `job started on another job's machine
   before one was launched for it`, and about 30 seconds later `launching a machine again
   for a job whose machine ran another job`. `truner doctor` reports a job that still
   waits five minutes after its machine was launched: then the control plane did not
   launch one again (see `truner logs --control-plane`). Cancel the run and re-run it,
   and give every job its own run token (`run=${{ github.run_id }}-<job>`, see
   [labels](labels.md#one-run-token-per-job)).
3. **App installation:** the GitHub App must be installed on the repository
   (`truner github install`).
4. **Webhooks:** `truner github status` lists recent deliveries. A `401` means the
   webhook secret does not match: run `truner install --new-github-app`.
5. **Launch errors:** `truner jobs` shows the job's status and `truner logs --control-plane`
   the worker's attempts. Common causes:
   - `VcpuLimitExceeded` / `MaxSpotInstanceCountExceeded`: raise quotas with
     `truner quota --request 512` (`--gpu` for GPU instances).
   - `no capacity`: rare instance types or a single family. Widen `family`/`type`.
   - `instance limit reached` or `monthly budget … exhausted`: see `limits` in
     `truner config show`.
   - A micro job's log warns `this micro job runs on an EC2 machine: …` with the reason,
     for example a MicroVM image that is still being built. See
     [MicroVMs](microvms.md#how-it-works).
6. **Machine started but no job:** read the machine's log with `truner logs <job-id>`.
   It contains the bootstrap, the agent's steps and the runner's output.

## The CLI does not know a label or an image

A CLI older than the installation does not know what came after it. A v0.1.1 CLI on a
v0.1.3 installation, for example, does not know the `micro` label in `truner explain`
and does not list MicroVM images in `truner images list`. Compare `truner version` with
the version `truner status` prints, and install the latest CLI:

```bash
curl -fsSL https://raw.githubusercontent.com/codician-team/truner/main/install.sh | sh
```

Current CLIs say so on every command when they are older than the installation, and
`truner upgrade` updates the CLI first. A CLI older than v0.1.4 deploys its own version
without asking, which takes away what came since: v0.1.1 removed the MicroVM roles from
a v0.1.3 installation. Installations deployed by v0.1.5 or newer are locked against
that: an older CLI's update fails with `Action denied by stack policy` and rolls back
without changing anything. To check which CLI runs, use `truner version`, and on macOS
and Linux `which -a truner`: the install script warns when another `truner` comes first
on the `PATH`.

If an installation must be changed outside truner, lift the lock for one update:
`aws cloudformation update-stack ... --stack-policy-during-update-body
'{"Statement":[{"Effect":"Allow","Action":"Update:*","Principal":"*","Resource":"*"}]}'`.

`install` and `upgrade` fail with a message about `cloudformation:SetStackPolicy` when
the AWS identity may not set stack policies. Add that permission, or use an identity
with administrator access. `truner doctor` warns `a truner CLI older than v0.1.5 can
still downgrade this installation` until the next `truner upgrade` locks it.

If `truner upgrade` cannot replace the CLI, for example because it has no write access
to the CLI's directory, it says so and changes nothing. Install the latest CLI with the
script above, or pass `--no-cli-update` to deploy the CLI's own version.

## A job fails right at the start with a truner error

The banner at the top of the job log explains it, for example an invalid label value
or a `.github/truner.yml` syntax error.

## Slow starts

- The first use of an image runs on the base AMI while the optimized image is built
  (`truner images list`). Later jobs are faster.
- `ubuntu24-full` installs many tools when not baked yet. Build it ahead of time:
  `truner images build ubuntu24-full --arch all`.
- For the fastest starts use a [warm pool](pools.md).
- The "Boot" line of the job banner shows where the time went. `truner logs <job-id>`
  adds the agent's steps and cloud-init's stage timings ("Up 24.5 seconds").
- **A baked image's first reads are slow.** EBS restores the volume of a custom AMI
  lazily: each block is fetched from S3 the first time it is read. That makes early boot
  on a baked image take 15 to 20 seconds longer than on Ubuntu's own image, whose
  blocks are already in use across EC2. truner works around most of it: the agent
  starts at network-up, and it skips walking the runner's files and reads them in
  parallel. Warm pools avoid it entirely, because a stopped machine keeps its volume.
  The `disk-init` label speeds up the background copy of the whole volume, at extra
  EBS cost.
- Machines with hard disk instance storage (`d3en` and others) are not chosen unless
  you name their family. They used to take minutes to set up their disks.
- **The first micro job of each size after an upgrade** builds its MicroVM image (3–4
  minutes) and runs on an EC2 Graviton machine meanwhile. See [MicroVMs](microvms.md).
- **`actions/checkout` is often the slowest step.** Every machine is new, so it fetches
  the repository from GitHub each time, and on EC2 that took anywhere from 10 to 100
  seconds for the same 237 MB repository. With a
  [repository snapshot](caching.md#repository-snapshots), the
  checkout step logs `truner: restored …` and fetches only newer objects. It logs
  `truner: no snapshot …` until a job on the default branch has saved one, and nothing
  for a full-history checkout (`fetch-depth: 0`).

## Cache misses

- The job banner says whether the magic cache is active. If the runner could not be
  patched (a new runner release changed its layout), it says so and falls back to
  GitHub's cache.
- The job-completed hook prints hit/miss counts at the end of every job.
- `truner cache ls --repo owner/name` lists stored entries.
- Caches are isolated by branch like on GitHub: a feature branch reads its own and
  its base branch's caches, never another feature branch's.

## Logs

| What | Where |
|---|---|
| Control plane | `truner logs --control-plane [-f] [--since 2h] [JOB-ID]` (CloudWatch `/truner/<stack>/webhook`, `worker` and `janitor`) |
| One machine | `truner logs <job-id>` (S3 `logs/<date>/<runner>.log`, uploaded when the machine ends) |
| Image builds | `s3://<bucket>/logs/bakes/<bake-id>.log` |
| MicroVM image builds and MicroVMs | CloudWatch `/truner/<stack>/microvms` (not read by `truner logs`) |
| Metrics | CloudWatch dashboard (`truner status` prints the link) |

## Removing a stuck installation

`truner uninstall` terminates machines and MicroVMs, deletes baked and MicroVM images,
empties the bucket and deletes the stack. If CloudFormation reports `DELETE_FAILED`, run
it again. Most often a machine was still shutting down.
