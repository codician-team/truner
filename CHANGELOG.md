# Changelog

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
