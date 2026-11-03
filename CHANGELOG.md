# Changelog

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
