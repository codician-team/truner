# Configuration

truner works without any configuration. Two optional files let you customize it:

| File | Scope | Edit with |
|---|---|---|
| Account config (`config/truner.yml` in the installation's bucket) | every repository | `truner config edit` / `truner config apply FILE` |
| `.github/truner.yml` in a repository | that repository | a normal commit |

Both are YAML. Unknown keys are rejected, so typos surface quickly. Validate files
locally with `truner config validate FILE` (add `--repo` for repository files).

A broken account config never stops jobs: the last valid version stays in use and
`truner doctor` reports the error. A broken repository config fails that repository's
jobs with the parse error shown in the job log. Changes to the account config apply to
new jobs within a minute.

## Repository config: `.github/truner.yml`

The file is read from the commit being built, so changes can be tested in a pull request.
`.github/truner.yaml` works too.

```yaml
# Settings for every job of this repository that runs on truner.
defaults:
  disk: 80
  env:
    CARGO_INCREMENTAL: "0"

# Named machine shapes: runs-on: truner/runner=<name>
runners:
  big:
    extends: 16cpu-linux-x64      # start from a built-in or another profile
    image: ubuntu24-full
    disk: 200
  arm:
    cpu: 8
    arch: arm64
    spot: only
  e2e:
    cpu: 4-8
    ram: 16
    family: [c7a, m7a, c7i, m7i]
    nvme: true
    timeout: 90

# Custom images for this repository: runs-on: truner/image=<name>
images:
  golden:
    name: "my-golden-ubuntu-*"   # newest AMI matching this name
    owner: "123456789012"
    profile: none                 # already contains everything
```

## Account config

```yaml
defaults:            # applied to every job (repo defaults and labels override)
  spot: true
  disk: 60

runners:             # shared profiles, usable from every repository
  big:
    extends: 32cpu-linux-x64
    image: ubuntu24-full

images:              # shared custom images
  company-base:
    ami: ami-0123456789abcdef0
    platform: linux
    archs: [x64]
    profile: standard   # the agent installs the standard tool set on boot
    bake: true          # truner bakes and maintains an optimized copy

pools:               # warm machines: runs-on: truner/pool=<name>
  fast:
    runner: 4cpu-linux-x64
    size: 3            # machines kept ready, 0–100
    maxAgeHours: 72    # recycle machines older than this (default 168, a week)

access:              # which repositories may use truner (glob patterns)
  repos: ["my-org/*"]
  deny: ["my-org/untrusted-*"]

limits:
  maxInstances: 100        # concurrent machines (default 100)
  monthlyBudgetUsd: 500    # hard stop: jobs wait once the month's estimate reaches this

autoBake: true             # maintain optimized AMIs for used images (default true)
bakeIntervalDays: 7        # rebuild baked images this often (default 7)
retryOnInterruption: true  # re-run jobs killed by spot interruptions (default true)
```

## Runner spec keys

Profiles (`runners:`), `defaults:` and pool definitions all accept these keys. The
limits are the same as for [labels](labels.md).

| Key | Type | Example |
|---|---|---|
| `extends` | profile name | `16cpu-linux-x64` |
| `cpu` | number, `"min-max"`, `[min, max]`, `"min+"` (1–896) | `8`, `"4-16"` |
| `ram` | same (GiB). A single number is a minimum | `32` |
| `family` | list or `a+b` | `[c7a, m7a]` |
| `type` | list | `[c7i.4xlarge]` |
| `arch` | `x64`/`arm64` | `arm64` |
| `gpu` | number (0–16) | `1` |
| `nvme` | bool | `true` |
| `micro` | bool: run on a Lambda [MicroVM](microvms.md) | `true` |
| `image` | image name | `ubuntu24-full` |
| `spot` | `true`/`false`/`only` | `only` |
| `disk` (8–16384), `diskIops` (3000–80000), `diskThroughput` (125–2000), `diskInitRate` (100–300) | numbers | `200` |
| `private`, `ssh` | bool | `true` |
| `cache` | `s3`/`github`/`off` | `s3` |
| `timeout` (up to 7200), `idleTimeout` (up to 1440), `keep` (up to 720) | minutes | `90` |
| `env` | map | `{ JAVA_OPTS: -Xmx8g }` |
| `tags` | map: extra EC2 tags (e.g. for cost allocation). Keys starting with `aws:` or `truner:` are ignored | `{ team: payments }` |

Pools also take `runner` (the profile to start from), `size` (0–100) and `maxAgeHours`
(default 168). Pool machines are always on-demand. Profile, image and pool names may
use letters, digits, `-`, `_` and `.`, up to 64 characters.

## Image spec keys

| Key | Meaning |
|---|---|
| `ami` / `name` + `owner` / `ssm` | exactly one source: an AMI id, the newest AMI matching a name pattern, or an SSM parameter (may contain `{arch}` → `amd64`/`arm64` and `{march}` → `x86_64`/`arm64`) |
| `platform` | `linux` (default) or `windows` |
| `archs` | supported architectures |
| `profile` | `none`, `standard` or `full`: what the agent installs when the image is not baked |
| `bake` | maintain an optimized AMI with the profile preinstalled |
| `os` | value of `ImageOS` for jobs |
| `user` | user that runs jobs (default `runner`) |

Custom images must run cloud-init (Linux) or EC2Launch v2 (Windows) so that user data
executes on boot. Ubuntu, Debian and Amazon Linux AMIs do.
