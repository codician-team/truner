# Security

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub's
[private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
on this repository rather than in a public issue. We aim to respond within a few days.

## Security model

truner runs entirely in your AWS account and talks only to GitHub and AWS. There is no
third-party service and no telemetry.

**Isolation between jobs.** Each job runs on a newly launched EC2 instance that is
terminated afterwards, or for `micro` jobs on its own Lambda MicroVM (a Firecracker VM)
that is terminated when the job ends. Runner registrations are just-in-time, single-use and scoped to
the job's repository, so a machine can only ever take a job of that repository.

**What a job can access.** Like any self-hosted runner, a job has root on its machine
(via passwordless sudo) and therefore also:
- the machine's IAM role: read/write to the installation's cache prefixes in S3, upload
  machine and image build logs, pull from every ECR repository in the account, push to
  the ECR cache repository, SSM core, plus any policies you attach with `--runner-policy`;
- the instance metadata, including the machine's (already consumed) user data.

A job on a MicroVM has root inside it too and gets the MicroVM execution role instead:
the same cache prefixes, log upload and `--runner-policy` policies, plus the
installation's MicroVM log group, but no ECR or SSM. It can reach the
agent's lifecycle hook port, which accepts the job's configuration only once.

**Trust boundary.** All repositories that use one installation share the cache bucket
and the machine role. A job of repository A could read or overwrite cache entries of
repository B. That includes the repository snapshots that `actions/checkout` starts
from, and git trusts the objects it restores from them. Therefore:

- Use **separate installations** (`truner install --stack <name> --label-prefix <prefix>`)
  for groups of repositories with different trust levels.
- **Do not run workflows triggered by pull requests from forks** of public repositories on
  self-hosted runners. GitHub's guidance applies to truner too. Restrict usage with
  `access.repos` in the account config.

**Control plane.**
- Webhooks are authenticated with HMAC-SHA256 (`X-Hub-Signature-256`). Unsigned or
  wrongly signed requests are rejected before any processing.
- The GitHub App's private key and webhook secret are stored as an SSM SecureString
  (KMS-encrypted) and only readable by the control plane role.
- The GitHub App is created through the control plane's `/github/setup` and
  `/github/callback` pages. They only work with a random 192-bit state that the CLI
  registers. Only its hash is stored, it is valid for one hour and it works once. The
  callback refuses an app that belongs to a different GitHub owner than the one requested,
  and it never replaces stored credentials unless `--new-github-app` was given. Treat the
  setup link like a password until it has been used.
- The control plane role can launch instances with the runner role only (`iam:PassRole`
  is scoped to it). Destructive EC2 actions (terminate, stop, modify, deregister,
  delete) are limited to resources tagged with the installation's name.
- MicroVMs carry no tags, and Lambda does not document which resources its MicroVM
  actions are authorized on, so the control plane's MicroVM permissions apply to the
  account. It only acts on MicroVMs started from its own images, deleting images is
  limited to its own image names, and `iam:PassRole` is limited to its two MicroVM roles.
- The GitHub App requests `administration: write` (required by GitHub to register
  repository runners), `actions: write` (to read jobs and re-run interrupted ones) and
  `contents: read` (for `.github/truner.yml`). Install it only on the repositories that
  need runners.

**Network.** The runner security group allows no inbound traffic unless you pass
`--ssh-cidr`. Shell access goes through SSM Session Manager. The data bucket blocks
public access, enforces TLS and is encrypted at rest. Instance volumes are encrypted.
Instance metadata requires IMDSv2. Every MicroVM has an HTTPS endpoint, as Lambda
requires for the lifecycle hooks; it only accepts requests with a token from
`lambda:CreateMicrovmAuthToken`, which truner never creates.

**Supply chain.** At boot, machines download the agent from your artifacts bucket
(pre-signed URL) and the runner from your S3 mirror or GitHub releases. Profile
installs use the official Ubuntu, Docker, GitHub CLI, AWS CLI, Node.js, Go and Rust
distribution channels. Baked images remove this dependency at job time. MicroVM images
are built from `public.ecr.aws/ubuntu/ubuntu:24.04` and Ubuntu's package archive.
