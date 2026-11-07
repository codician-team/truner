# Changelog

## Unreleased

- **`truner costs` and `truner status` show what AWS billed.** A job's cost starts as an
  estimate, and AWS Cost Explorer's number replaces it about a day after the day ended.
  `truner jobs` marks estimates with `~`. EC2 counts only with the `truner:stack` tag, so
  other EC2 in the account stays out, even of the same instance type: run
  `truner costs --activate-tags` once per account. MicroVMs cannot be tagged and are split
  between the region's truner stacks. Needs `ce:GetCostAndUsage`, and reads the bill at most
  every 12 hours ($0.01 per request); `--estimates-only` does not read it.
- **Better cost estimates.** Measured on the AWS bill of 304 MicroVM jobs, the old estimate
  was 30% too high: it left out bursting, which costs 1.33 times the baseline, but counted
  3.1 times the snapshot data jobs really read. It is now within 3% for those days. EC2
  estimates use the spot price of the machine's own availability zone (the zone was never
  recorded, so a price from any zone was used), and count public IPv4 addresses and
  provisioned disk IOPS and throughput.
- **The comparison with GitHub-hosted runners is like for like.** It uses GitHub's list
  price per runner size instead of a rate per core, the cores of the job's machine (a pinned
  `type=c8a.4xlarge` counted as 2), and Arm64 prices at the peak size for MicroVMs (they were
  priced as x64 at the baseline).
- **Node 26 starts on MicroVMs.** Node's official Arm64 build links `libatomic.so.1`,
  which the MicroVM image did not have, so `node` exited with status 127 (finding 8 of
  the [MicroVMs field report](docs/field-report-microvms.md)). The image now installs
  `libatomic1`; it is rebuilt with the upgrade. Remove the `apt-get install libatomic1`
  workaround step afterwards, which cost each job about 10 s. EC2 machines already have
  it through `build-essential`.
- **`truner jobs` cuts long names between characters.** It cut the WORKFLOW / JOB
  column after 39 bytes, which split letters such as `ş` and `ğ` and printed invalid
  UTF-8. It now counts characters.
- **Machines that take no job leave evidence.** When a job ended before pickup, the
  webhook terminated its machine and no log was uploaded: job 127285209781 sat 602 s on
  a live machine, held up a deploy group and left nothing to diagnose. The control plane
  now keeps the console output of an EC2 machine it terminates before it took its job,
  as `logs/<date>/<runner>.console.log`, and `truner logs <job-id>` prints it. The agent
  also uploads its log while the runner waits for a job, after a minute and then every 5
  minutes, and right away when the machine shuts down. The control plane needs
  `ec2:GetConsoleOutput`, which `truner upgrade` adds.
- **Docker jobs start faster.** In one repository's builds, `docker/setup-buildx-action`
  took 27 s at the median: about 18 s for the first `docker version` and `docker info`,
  which read Docker's binaries from a lazily restored volume, and about 9 s to pull
  `moby/buildkit:buildx-stable-1`. The agent now reads Docker's binaries and runs
  `docker info` in the background while the runner waits for its job, and baked images
  and warm pool machines come with the BuildKit image. Baked images get it when they are
  rebuilt after the upgrade. On machines with local NVMe, the image is copied there with
  Docker's other data, which adds a moment to the agent's `docker` step.
- **Old instance generations are left out by default.** With `cpu=N` and no `family`,
  EC2 Fleet often picked the cheapest spot pool from 2018: at the median `CI / frontend`
  took 345 s on `r5a.2xlarge` against 148 s on `c7a.2xlarge`, and `CI / security` 233 s
  on `r5a.xlarge` against 78 s on `m8azn.xlarge`. Jobs without `family` or `type` no
  longer get `m5*`, `c5*`, `r5*`, `z1d`, `i3*` or `x1*`. Name the family to use one.
  `truner explain` shows what is left out.
- **Re-runs after a spot interruption stop when they cannot work.** After someone re-ran
  a run by hand, the janitor tried to re-run job 126466730494 749 times in 24 hours, each
  answered with `403 The workflow run containing this job is already running`. It now
  asks GitHub first: it waits without posting while the run is in progress, leaves the
  job alone when the run has a newer attempt, and stops when GitHub refuses a completed
  run's re-run.
- **`truner doctor` reports failed janitor tasks.** It said "Everything looks good" while
  the janitor failed 80 times in five days. It now reads the janitor's log and lists each
  task that failed in the last 24 hours, with the count and the last error. A failure in
  the last hour counts as a problem.
- **Far fewer `ListMicrovms` calls.** Two installations in one account logged 162 `could
  not count MicroVMs` warnings and 80 failed janitor runs with `ThrottlingException` in
  five days. The API lists the whole account's MicroVMs, terminated ones too, 10 per
  page, so one listing took 11 calls. Launches now count MicroVMs from truner's job
  records and never list them, the check after a job takes another job's machine does
  not list either, and the janitor lists 50 per page, only while a MicroVM is up and
  every 15 minutes otherwise. After throttling it waits 4, 8 and then 16 minutes,
  across all of the installation's Lambda functions. A failed list no longer leaves
  MicroVMs out of `limits.maxInstances`.
- **`~/.docker` belongs to the runner user.** The agent's ECR login created the folder as
  root, so `docker/setup-buildx-action` failed with `EACCES: permission denied, mkdir
  '/home/runner/.docker/buildx/.bin/...'`. Remove any `DOCKER_CONFIG` workaround from
  your workflows after upgrading.
- **A micro job that asks for x64 fails with a reason.** `truner/micro/arch=x64`, a runner
  profile with `arch: x64` or a runner name ending in `-x64` ran on an Arm64 MicroVM,
  where x64-only tools break in less obvious ways. Such jobs now fail with "micro
  machines are arm64 only; remove micro or arch=x64". An `arch: x64` default in the
  account or repository config still lets jobs use `micro`.
- **Jobs wait an hour for EC2 vCPU quota, as intended.** Each quota retry counted as a
  launch attempt, so a job gave up after about 15 minutes while the alert email said it
  would keep retrying. Quota retries now run every minute without using up attempts,
  until the job has waited an hour since it was queued.
- New page: [Field report: MicroVMs](docs/field-report-microvms.md), what moving one
  repository's CI onto MicroVMs with v0.1.3 to v0.1.5 found, with suggested improvements.

## v0.1.5

- **`truner upgrade` updates the CLI first.** A v0.1.1 CLI on a Mac ran `truner upgrade`
  on two v0.1.3 installations and deployed v0.1.1: it said "it will be upgraded",
  removed the MicroVM roles and reported "healthy (version v0.1.1)". Now, like
  `bun upgrade`, `upgrade` and `install` download the latest release when it is newer,
  check it against the release's checksums, replace the CLI and run the command again
  with it. `--cli-only` updates only the CLI, `--no-cli-update` deploys the CLI's own
  version.
- **Installations are locked against older CLIs.** The stack gets a CloudFormation stack
  policy that denies every update, and the CLI lifts it for its own update only. A CLI
  older than v0.1.5 cannot lift it, so its update fails and rolls back without changing
  anything. The AWS identity that runs truner needs `cloudformation:SetStackPolicy`.
  `truner doctor` reports installations that are not locked yet.
- **Clear versions in `install` and `upgrade`.** Step 1 says "it will be upgraded from
  v0.1.3 to v0.1.5" (or redeployed, or DOWNGRADED with `--allow-downgrade`). The end
  says "Installation "truner" is upgraded from v0.1.3 to v0.1.5", with links to the
  release notes, the issue tracker and the commit log, and warns when the control plane
  reports another version than the one deployed. `truner version` and `truner doctor`
  say when a newer release is out.
- **The install scripts warn about another `truner` on the `PATH`.** They print the
  version they installed, and warn when another `truner` comes first on the `PATH` and
  would run instead.

## v0.1.4

- **Repository snapshots for `actions/checkout`.** Every machine fetched the whole
  repository from GitHub, which took 11–102 seconds for a 237 MB repository on EC2
  ([#4](https://github.com/codician-team/truner/issues/4)). After a job on the default
  branch, the agent now saves the checked-out commit's git objects to the magic cache.
  A `git` wrapper on the runner's `PATH` restores them before checkout's shallow fetch
  into a new repository, which then transfers only newer objects. With a snapshot 60
  commits behind, the fetch of `vitejs/vite` received 203 KiB instead of 12.3 MiB.
  Snapshots are scoped by branch like cache entries and need no token. They work on
  Linux machines and MicroVMs, and `cache=off` turns them off.
- **Jobs with another installation's label prefix are reported.** A job labelled
  `truner/…` on an installation whose prefix is `truner-acme` waited for hours with no
  machine, while `truner doctor` said everything was fine and the control plane logged
  nothing. The webhook now logs such jobs and keeps them for two days. `truner doctor`
  lists the ones GitHub still has queued, with the label to use, `truner status` prints
  the installation's prefix, and `truner explain` warns about a label with another
  prefix. CLI hints such as the one in `truner jobs` use the installation's prefix
  instead of `truner/`.
- **A job left without a machine gets one.** Jobs with equal labels can take each
  other's machines. When a job started on a machine launched for another job before a
  machine was launched for itself, one job with those labels waited with no machine
  until it was cancelled ([#3](https://github.com/codician-team/truner/issues/3)). That
  happened when GitHub delivered the job's `queued` event late, and when the job's own
  launch was waiting for capacity, the instance limit or the budget. The control plane
  now launches a machine again for the job left waiting, about 30 seconds later and on
  every janitor run, once GitHub confirms that the job is still queued and no other
  machine with its labels is coming. The machine it had still books its cost to the job
  that ran on it, and is ended when that job is over.
- The janitor no longer ends the MicroVM of a cancelled job while another job with the
  same labels runs on it.
- **One run token per job.** All examples and docs now use
  `run=${{ github.run_id }}-<job>` (with the matrix value in matrix jobs), and so do
  truner's own workflows. A token per job keeps every job on the machine launched for it.
  `truner doctor` reports a job whose machine ran another job and that still waits.
- **CLI and installation versions.** A CLI older than the installation now warns on
  every command, because it may not know the installation's labels and images (a v0.1.1
  CLI does not know `micro`). `truner doctor` and `truner status` no longer tell an older
  CLI to run `truner upgrade`, and `upgrade` and `install` refuse to deploy an older
  version over a newer one without `--allow-downgrade`.
- New guide: [Splitting CI into parallel jobs](docs/parallel-jobs.md), with measured
  start and checkout times, hidden ordering dependencies, artifacts against rebuilding,
  and whole-core instance families for timing-sensitive tests.

## v0.1.3

- **MicroVMs (beta).** The `micro` label (or `micro: true` in a runner profile) runs a job
  on a Lambda MicroVM with 1, 2 or 4 vCPU instead of EC2. A MicroVM resumes from a
  snapshot with the runner already in memory, so short jobs such as linters and checks
  skip the machine boot. truner builds the MicroVM images on first use and after
  upgrades. Until an image is ready, and whenever a MicroVM cannot run a job, the job
  runs on an EC2 Graviton machine of the same size. MicroVMs are Arm64 only and have no
  Docker, and jobs run as root there. `truner upgrade` adds the MicroVM roles and log
  group to existing installations. In a first test in us-east-1, micro jobs were running
  6–9 seconds after they were queued.
- The runner prefetch reads only the runner and the Node.js build that actions run on,
  not every bundled Node.js, so it competes less with the job for the volume.
- Workflows and documentation examples use the current major versions of the GitHub
  actions, which run on Node.js 24.

## v0.1.2

- Faster job starts on baked images. The agent starts from its own systemd unit as soon
  as the network is up instead of after cloud-init's later stages. It no longer walks the
  whole runner directory before each job, and it reads the runner's files in parallel
  while it prepares the machine. In-use images are rebuilt after an upgrade, so the
  changes reach them.
- Instance types with hard disk instance storage (`d2`, `d3`, `d3en`, `h1`) are no longer
  chosen automatically. Their disks are never used for the work directory, which took
  minutes to set up.
- Measured on codician's own CI: jobs now start 44 to 66 seconds after they are queued,
  instead of 1m15s to 3m15s, and the test job runs in 2m33s instead of 8m20s.
- truner's own CI, docs and releases run on truner. Pull requests from forks keep
  GitHub-hosted runners.

## v0.1.1

- Magic cache: a key that is already stored or still uploading is now refused with twirp
  `already_exists` (HTTP 409), like GitHub. Parallel `docker buildx` builds with
  `type=gha` cache exports no longer fail with "failed to reserve cache".

## v0.1.0

First release.

- One-command installation (`truner install`): CloudFormation control plane, GitHub App
  via the manifest flow, guided AWS credential setup.
- Ephemeral EC2 runners per job: spot with on-demand fallback via EC2 Fleet,
  attribute-based instance selection, x64/arm64/GPU/Windows.
- Label syntax (`truner/cpu=4/ram=16/...`), built-in runner profiles, repository and
  account configuration files.
- Magic cache: GitHub cache protocols (v2 and legacy v1) backed by S3 with branch scoping.
- Docker layer caching via ECR and S3, local NVMe RAID setup.
- Self-maintaining baked AMIs (standard and full profiles), custom images.
- Warm pools of stopped machines.
- Self-healing: webhook redelivery, relaunch of dead machines, spot-interruption
  re-runs, deadlines, idle shutdown.
- Cost tracking per job, monthly budget hard stop, alerts and daily reports.
- Documentation website (VitePress, deployed to GitHub Pages) with a landing page.
- Agent-ready installation: `docs/ai-agents.md` guides AI coding agents through the
  whole install, and the website serves `llms.txt`, `llms-full.txt` and every page as
  Markdown. The installer never prompts without a terminal, marks user steps with
  `ACTION REQUIRED` and resumes when it is run again.
- GitHub App setup through the control plane: the setup link works on any device and
  survives an interrupted install. `truner login` saves AWS credentials on its own.
- CLI: status, doctor, jobs, costs, logs, explain, images, cache, config, pools,
  connect, quota, github, upgrade, uninstall.
