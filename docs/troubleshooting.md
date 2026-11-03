# Troubleshooting

Start with:

```bash
truner doctor
```

It checks credentials, the stack, the control plane, EC2 quotas, the GitHub App
(permissions, events, installations), recent webhook deliveries and recent launch
failures, and prints the fix for each problem.

## A job stays queued

1. **Labels:** the first label must be `truner` (or your `--label-prefix`). Check what
   a label resolves to with `truner explain <label>`.
2. **App installation:** the GitHub App must be installed on the repository
   (`truner github install`).
3. **Webhooks:** `truner github status` lists recent deliveries. A `401` means the
   webhook secret does not match: run `truner install --new-github-app`.
4. **Launch errors:** `truner jobs` shows the job's status and `truner logs --control-plane`
   the worker's attempts. Common causes:
   - `VcpuLimitExceeded` / `MaxSpotInstanceCountExceeded`: raise quotas with
     `truner quota --request 512` (`--gpu` for GPU instances).
   - `no capacity`: rare instance types or a single family. Widen `family`/`type`.
   - `instance limit reached` or `monthly budget … exhausted`: see `limits` in
     `truner config show`.
5. **Machine started but no job:** read the machine's log with `truner logs <job-id>`.
   It contains the bootstrap, the agent's steps and the runner's output.

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
| Control plane | `truner logs --control-plane [-f] [--since 2h] [JOB-ID]` (CloudWatch `/truner/<stack>/…`) |
| One machine | `truner logs <job-id>` (S3 `logs/<date>/<runner>.log`, uploaded when the machine ends) |
| Image builds | `s3://<bucket>/logs/bakes/<bake-id>.log` |
| Metrics | CloudWatch dashboard (`truner status` prints the link) |

## Removing a stuck installation

`truner uninstall` terminates machines, deletes baked images, empties the bucket and
deletes the stack. If CloudFormation reports `DELETE_FAILED`, run it again. Most often
a machine was still shutting down.
