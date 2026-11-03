# Images

## Built-in images

| Name | OS | Arch | Contents |
|---|---|---|---|
| `ubuntu24` (default) | Ubuntu 24.04 | x64, arm64 | **standard** profile |
| `ubuntu22` | Ubuntu 22.04 | x64, arm64 | standard |
| `ubuntu24-full` | Ubuntu 24.04 | x64, arm64 | **full** profile |
| `ubuntu22-full` | Ubuntu 22.04 | x64, arm64 | full |
| `ubuntu24-gpu`, `ubuntu22-gpu` | AWS Deep Learning Base OSS NVIDIA Driver AMI | x64 | NVIDIA driver, CUDA, Docker + NVIDIA container toolkit, plus standard |
| `windows2025`, `windows2022` | Windows Server Full Base | x64 | Git for Windows (with bash), PowerShell 7 |

Aliases: `ubuntu-latest`, `ubuntu-24.04`, `ubuntu-22.04`, `windows-latest`, `windows-2025`,
`windows-2022`, `gpu`. Add `-arm64` or `-x64` to choose the architecture, for example
`image=ubuntu24-full-arm64`.

**standard:** Docker CE with buildx and compose, git, git-lfs, GitHub CLI, AWS CLI v2,
build-essential, Python 3 with pip/venv, jq, yq, zstd, zip/unzip, rsync, openssh-client and
common build libraries. Jobs run as `runner` with passwordless sudo, like on GitHub-hosted
runners. `setup-*` actions install language versions into `/opt/hostedtoolcache`.

**full:** standard plus Node.js LTS (npm, yarn, pnpm), Go, Rust (rustup, clippy,
rustfmt), Java 17 and 21 (Maven, Ant), .NET 8 SDK, Ruby, clang/LLVM, CMake, Ninja,
Meson, kubectl, Helm, kind, Terraform, Packer, Poetry, pipx, ShellCheck, PostgreSQL,
MySQL and Redis clients, ImageMagick, FFmpeg. This is close to, but not the same as,
GitHub's `ubuntu-latest`.

## Baked images (automatic)

The first time a bakeable image is used in an architecture, truner:

1. Runs the job on the **base** AMI right away. The agent installs the profile during
   boot: about 60 s for standard, several minutes for full.
2. Starts a **builder** in the background that installs the profile, pre-downloads
   the runner, cleans up and stops. The janitor turns it into an AMI tagged for your
   installation.
3. Uses that AMI for all later jobs.

On machines from a baked image, the agent starts from its own systemd unit as soon as
the network is up. It doesn't wait for cloud-init's later stages, which wait for snapd
and take 5 to 20 seconds on a fresh volume. The user data script still runs
afterwards. It finds the agent already running and does nothing, unless the image is
older than your installation, in which case it downloads the matching agent and
starts it.

Images are rebuilt every `bakeIntervalDays` (default 7) while they are in use, after an
upgrade to a new truner version, and on demand. The newest two are kept. Manage them with:

```bash
truner images list
truner images build ubuntu24-full --arch all
truner images delete ami-0123456789abcdef0
```

`truner install` pre-builds `ubuntu24` for x64 and arm64 unless `--no-bake` is given.
Disable baking entirely with `autoBake: false` in the account config.

## Custom images

Bring your own AMI (for example built with Packer from
[actions/runner-images](https://github.com/actions/runner-images) for full
GitHub parity):

```yaml
# account config or .github/truner.yml
images:
  gh-parity:
    name: "runner-images-ubuntu-24.04-*"
    owner: "self"
    profile: none     # nothing to install at boot
    os: ubuntu24
```

```yaml
runs-on: truner/image=gh-parity/cpu=8
```

Requirements: cloud-init (Linux) or EC2Launch v2 (Windows), `bash`, `curl` and
`systemd`. The agent creates the `runner` user if needed and installs the GitHub
runner itself. With `profile: standard` and `bake: true` truner layers the
standard tools on top of your AMI and maintains the result.

## GPU

```yaml
runs-on: truner/runner=gpu-nvidia
steps:
  - run: nvidia-smi
  - run: docker run --rm --gpus all nvidia/cuda:12.6.0-base-ubuntu24.04 nvidia-smi
```

New AWS accounts often have a GPU vCPU quota of 0. Check with `truner quota` and raise
it with `truner quota --gpu --request 64`.

## Windows

```yaml
runs-on: truner/runner=4cpu-windows-x64
defaults:
  run:
    shell: pwsh
```

Windows machines take 3–5 minutes to boot and install Git and PowerShell 7 at boot.
Jobs run as `LocalSystem`. Docker, SSH access and warm pools are Linux-only. Baking
Windows images (`bake: true` on a Windows image) is experimental.
