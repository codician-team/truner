# Install with an AI agent

An AI coding agent such as Claude Code, Codex, Cursor or GitHub Copilot can install
truner for you. Paste this prompt into an agent that can run commands on your computer:

```text
Install truner in my AWS account so my GitHub Actions jobs run on it.
Follow https://codician-team.github.io/truner/ai-agents.md
```

The agent installs the CLI, deploys truner to your AWS account and checks the result.
GitHub requires a person for two clicks, and the agent will ask you for them:

1. **Create GitHub App**, on a GitHub page the agent links to.
2. **Install**, after you choose the repositories that should use truner.

If no AWS credentials are set up on your computer, the agent asks you to run
`truner login` in your own terminal and paste an access key there, so the secret never
passes through the agent. The agent asks before it changes your workflows.

## Instructions for AI agents

You are installing truner: self-hosted, ephemeral GitHub Actions runners in the user's
own AWS account. Follow these steps in order. Tell the user briefly what you are doing
at each step.

### Rules

- **Never ask the user to paste AWS secret keys into the chat.** If credentials are
  missing, ask them to run `truner login` in their own terminal.
- **Ask before** you change workflow files, request quota increases
  (`truner quota --request`), add `--nat` (about $32 per month) or run `truner uninstall`.
- truner creates a CloudFormation stack named `truner` in the user's AWS account. It
  costs close to nothing while idle. Jobs cost the EC2 price of the seconds they run.
- truner never prompts when standard input is not a terminal. Pass everything as flags.
  Every command exits with a non-zero code when it fails.

### 1. Install the CLI

Check whether it is installed with `truner version`. An older CLI is fine:
`truner install` and `truner upgrade` update it to the latest release first. If it is
not installed:

```bash
# macOS and Linux (installs to /usr/local/bin or ~/.local/bin)
curl -fsSL https://raw.githubusercontent.com/codician-team/truner/main/install.sh | sh
```

```powershell
# Windows PowerShell (installs to %LOCALAPPDATA%\truner\bin and adds it to the user's PATH; open a new shell afterwards)
irm https://raw.githubusercontent.com/codician-team/truner/main/install.ps1 | iex
```

The script prints the version it installed and where. If `truner` is not found after
installing, use that full path. If the script warns that another `truner` comes first on
the `PATH`, that one would run instead: use the full path, and tell the user which file
to remove.

### 2. Choose the AWS account and region

truner uses the standard AWS credential chain: environment variables, `AWS_PROFILE`,
`~/.aws` profiles and SSO. It also uses the profile that `truner login` saves.

- If the AWS CLI is installed, `aws sts get-caller-identity` shows which account the
  current credentials belong to. Confirm the account with the user. If they have several
  profiles, ask which one to use and pass it as `--profile NAME`.
- Region: use the region that the user's other AWS resources are in
  (`aws configure get region`), or ask. Without one, use `us-east-1`.
- If there are no credentials, ask the user to run `truner login` in their own terminal.
  It asks for an access key of an IAM user with administrator access and saves it as
  the AWS profile `truner`. Continue when they confirm.
- The credentials need administrator access. A narrower policy must still allow
  `cloudformation:SetStackPolicy`, which truner uses on every install and upgrade.
- If `truner status` already finds an installation in that account and region, go to
  [Upgrading an existing installation](#upgrading-an-existing-installation) instead.

### 3. Choose the GitHub owner

truner creates a private GitHub App for one organization or for the user's personal
account. Only repositories of that owner can use it.

- Inside a git repository, the owner is the first part of
  `git remote get-url origin` (for example `my-org` in `github.com/my-org/app`).
  Confirm it with the user.
- With the GitHub CLI, `gh api users/OWNER --jq .type` prints `Organization` or `User`.
- For an organization, pass `--github-owner ORG`. The user must be an owner of the
  organization to create the app. For a personal account, leave out `--github-owner`.

### 4. Run the installer

```bash
truner install --yes --region REGION --github-owner ORG
```

The deployment takes 3–5 minutes, plus the time the user needs on GitHub. Run the
command in the background, or with a timeout of at least 15 minutes, and read its
output while it runs.

- When a line starts with **`ACTION REQUIRED`**, show the user the link on the next line
  right away and tell them what to do. The first one asks them to open the link and
  click **Create GitHub App**. GitHub then shows the installation page, where they choose
  the repositories and click **Install**. The link works on any device, including a
  phone.
- The installer notices both steps by itself and continues. It ends with
  `truner is ready!` and exit code 0.
- If a newer CLI is out, the installer first prints `truner vX.Y.Z is out!`, updates
  itself and runs the same command again. That is expected.
- If the command is interrupted, for example by your timeout, run exactly the same
  command again. It continues where it stopped and reuses the same link for an hour.
- If it fails with `no AWS credentials found`, go back to step 2.

### 5. Verify

```bash
truner doctor
```

Exit code 0 means everything works. Otherwise every problem comes with the command that
fixes it. New AWS accounts often have a low EC2 vCPU quota. If `doctor` reports one,
tell the user and offer to run `truner quota --request 512`. Ask first, because it files
a request with AWS.

### 6. Move workflows to truner (ask first)

Offer to switch workflows in the user's repositories. Only `runs-on` changes:

| Currently | With truner |
|---|---|
| `ubuntu-latest`, `ubuntu-24.04` | `truner/cpu=4/run=${{ github.run_id }}-<job>` |
| `ubuntu-22.04` | `truner/cpu=4/image=ubuntu22/run=${{ github.run_id }}-<job>` |
| `ubuntu-24.04-arm` | `truner/runner=4cpu-linux-arm64/run=${{ github.run_id }}-<job>` |
| larger runners, for example `ubuntu-latest-8-cores` | `truner/cpu=8/run=${{ github.run_id }}-<job>` |
| `windows-latest` | `truner/runner=4cpu-windows-x64/run=${{ github.run_id }}-<job>` (beta, boots in 3–5 minutes) |
| `macos-*` | keep it: macOS is not supported |

- The default image has Docker, git, the GitHub CLI, the AWS CLI, Python and build
  tools. `setup-*` actions work as usual. If a job uses tools without installing them
  (Node.js, Java, Go or .NET without a `setup-*` step), use `image=ubuntu24-full`.
- Use the installation's label prefix in place of `truner` if it was installed with
  `--label-prefix` (`truner status` prints it). A job with another prefix stays queued
  with no machine and no error.
- Give every job its own run token: replace `<job>` with the job's id, and add the
  matrix value in a matrix (`run=${{ github.run_id }}-test-${{ matrix.shard }}`). The run
  id stops machines from taking jobs of other workflow runs. The job's name stops jobs of
  the same run with equal labels from taking each other's machines, which can leave one
  job waiting for a second launch. `github.job` is empty in `runs-on`, so write the name
  out.
- For `runs-on: ${{ matrix.os }}`, change the matrix values instead.
- Tests with CPU-time or wall-clock bounds: pin whole-core families, for example
  `family=m7a+m8a+r7a+r8a` ([why](labels.md#machine)).
- Do not move jobs that run on `pull_request` events from forks of public repositories.
  Self-hosted runners should not run untrusted code.
- Make the change on a branch, push it, and watch the job start with
  `truner jobs --watch`. Open a pull request if the user wants one.

Check what a label resolves to with `truner explain truner/cpu=8/ram=32`.
Full reference: [labels](labels.md).

### 7. Report back

Tell the user:

- the AWS account and region truner runs in, and the GitHub App's name
- which workflows now run on truner
- `truner jobs --watch` shows running jobs and `truner costs` compares spend with
  GitHub-hosted runners
- the docs: https://codician-team.github.io/truner/

## Upgrading an existing installation

When the user asks to update truner, or truner is already installed:

```bash
truner upgrade --yes
```

- It updates the CLI to the latest release first and runs again with it. Step 1 then
  says what happens, for example `it will be upgraded from v0.1.3 to v0.1.5`. The end
  says `Installation "truner" is upgraded from v0.1.3 to v0.1.5.` Tell the user both
  versions and pass on the release notes link it prints.
- The deployment takes 2–4 minutes. Pass `--stack NAME` for an installation that is not
  named `truner`, and the same `--profile` and `--region` as for the install.
- If the CLI cannot replace itself (for example, no write access to its directory), it
  prints an `ACTION REQUIRED` line with the install script's command and changes
  nothing. Run the install script from step 1, then `truner upgrade --yes` again.
- Never pass `--allow-downgrade` unless the user asks to go back to an older version.
- Run `truner doctor` afterwards (step 5).

## Troubleshooting for agents

| Output | What to do |
|---|---|
| `no AWS credentials found` | Ask the user to run `truner login` in their own terminal, then rerun. |
| `AccessDenied` or `not authorized` during deployment | The credentials need administrator access (IAM, EC2, Lambda, CloudFormation including `cloudformation:SetStackPolicy`, S3, DynamoDB, SQS, SSM, SNS, ECR, CloudWatch). Ask the user for other credentials. |
| `the installation runs vX, newer than this CLI` | An old CLI runs, often another `truner` earlier on the `PATH`. Install the latest CLI (step 1) and rerun. Do not pass `--allow-downgrade` unless the user asks. |
| `Action denied by stack policy` | A CLI older than v0.1.5 tried to change the installation and nothing changed. Install the latest CLI (step 1) and rerun. |
| `This setup link has expired` in the browser | Run the same `truner install` command again for a new link. |
| `the app ... belongs to X, not Y` | The app was created for the wrong account. The user deletes it with the link shown, and you rerun with the right `--github-owner`. |
| `a GitHub App is already configured` | An app exists already. Rerun without changes. Add `--new-github-app` only if the user wants to replace it. |
| The installer says `the app is not installed yet` | The user still has to click **Install** at the link shown. `truner github install` prints it again. |
| A job stays queued | Run `truner doctor`, then `truner logs --control-plane`. See [troubleshooting](troubleshooting.md). |

Every command supports `--help`. The full CLI reference is in [cli](cli.md).

## Removing truner

Only when the user asks for it:

```bash
truner uninstall --yes
```

It terminates all machines and deletes the stack, the baked and MicroVM images and the
cache. The user then deletes the GitHub App in their GitHub settings, because GitHub has
no API for removing apps.
