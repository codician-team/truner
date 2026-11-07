#!/bin/bash
# truner "standard" profile: what most CI jobs expect on a Linux runner.
# Runs as root on Ubuntu 22.04/24.04 (x64 and arm64). Idempotent.
set -uo pipefail
export DEBIAN_FRONTEND=noninteractive
RUNNER_USER="${TRUNER_USER:-runner}"
ARCH="$(dpkg --print-architecture)"
. /etc/os-release
APT=(apt-get -o DPkg::Lock::Timeout=600 -o Acquire::Retries=5 -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -yq)
mkdir -p /etc/truner /etc/apt/keyrings
touch /etc/truner/extra-path /etc/truner/extra-env

warn() { echo "::truner-warning:: $*"; }
try() { "$@" || warn "failed: $*"; }
aptget() {
  "${APT[@]}" install "$@" && return 0
  warn "bulk install failed, retrying packages one by one"
  local p
  for p in "$@"; do "${APT[@]}" install "$p" >/dev/null 2>&1 || warn "package $p failed"; done
}

cat >/etc/apt/apt.conf.d/99truner <<'EOF'
APT::Install-Recommends "0";
APT::Install-Suggests "0";
Acquire::Languages "none";
Acquire::http::Pipeline-Depth "10";
EOF

# Third-party repositories first so a single `apt-get update` covers everything.
if ! command -v docker >/dev/null 2>&1; then
  if curl -fsSL --retry 5 https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc; then
    chmod a+r /etc/apt/keyrings/docker.asc
    echo "deb [arch=${ARCH} signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" >/etc/apt/sources.list.d/docker.list
  fi
fi
if ! command -v gh >/dev/null 2>&1; then
  if curl -fsSL --retry 5 https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli-archive-keyring.gpg; then
    chmod a+r /etc/apt/keyrings/githubcli-archive-keyring.gpg
    echo "deb [arch=${ARCH} signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" >/etc/apt/sources.list.d/github-cli.list
  fi
fi

"${APT[@]}" update || { sleep 10; "${APT[@]}" update; }

aptget ca-certificates curl wget gnupg git git-lfs jq unzip zip xz-utils zstd pigz \
  build-essential pkg-config python3 python3-pip python3-venv rsync openssh-client sudo acl file \
  time tree lsb-release apt-transport-https software-properties-common bc dnsutils iputils-ping \
  net-tools mdadm nvme-cli parallel tzdata locales gettext-base libssl-dev libffi-dev libicu-dev

if ! command -v docker >/dev/null 2>&1; then
  "${APT[@]}" install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin \
    || "${APT[@]}" install docker.io docker-buildx docker-compose-v2 \
    || "${APT[@]}" install docker.io \
    || warn "docker installation failed"
fi
if command -v docker >/dev/null 2>&1; then
  try systemctl enable --now docker
  getent group docker >/dev/null && usermod -aG docker "$RUNNER_USER"
  # Baked images and pool machines keep the image docker/setup-buildx-action
  # starts BuildKit from, so buildx only checks its tag instead of pulling it.
  if [ -n "${TRUNER_PRELOAD:-}" ]; then
    try docker pull -q moby/buildkit:buildx-stable-1
  fi
fi
command -v gh >/dev/null 2>&1 || "${APT[@]}" install gh || warn "gh installation failed"

# AWS CLI v2
if ! command -v aws >/dev/null 2>&1; then
  case "$ARCH" in amd64) A=x86_64 ;; arm64) A=aarch64 ;; esac
  tmp="$(mktemp -d)"
  if curl -fsSL --retry 5 "https://awscli.amazonaws.com/awscli-exe-linux-${A}.zip" -o "$tmp/awscliv2.zip"; then
    unzip -q "$tmp/awscliv2.zip" -d "$tmp" && "$tmp/aws/install" --update >/dev/null || warn "aws cli install failed"
  fi
  rm -rf "$tmp"
fi

# yq
if ! command -v yq >/dev/null 2>&1; then
  try curl -fsSL --retry 5 "https://github.com/mikefarah/yq/releases/latest/download/yq_linux_${ARCH}" -o /usr/local/bin/yq
  chmod 0755 /usr/local/bin/yq 2>/dev/null || true
fi

try git lfs install --system --skip-repo
git config --system --add safe.directory '*' || true

# Tool cache used by actions/setup-* (same path as GitHub-hosted runners).
mkdir -p /opt/hostedtoolcache
chown "$RUNNER_USER:$RUNNER_USER" /opt/hostedtoolcache 2>/dev/null || true

cat >/etc/sysctl.d/99-truner.conf <<'EOF'
fs.inotify.max_user_watches = 655360
fs.inotify.max_user_instances = 1280
fs.file-max = 2097152
vm.max_map_count = 262144
net.core.somaxconn = 4096
# Allow unprivileged user namespaces (Chrome/Playwright sandboxes, rootless
# containers), as on GitHub-hosted runners. Ignored on kernels without it.
kernel.apparmor_restrict_unprivileged_userns = 0
EOF
sysctl --system >/dev/null 2>&1 || true

cat >/etc/security/limits.d/99-truner.conf <<'EOF'
*    soft nofile 65536
*    hard nofile 1048576
root soft nofile 65536
root hard nofile 1048576
EOF

# Runners are disposable: never let background upgrades grab the dpkg lock.
for u in apt-daily.timer apt-daily-upgrade.timer unattended-upgrades.service motd-news.timer man-db.timer; do
  systemctl disable --now "$u" >/dev/null 2>&1 || true
done
echo "standard profile done"
