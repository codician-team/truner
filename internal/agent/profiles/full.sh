#!/bin/bash
# truner "full" profile: language toolchains and tools commonly preinstalled
# on GitHub-hosted runners. Runs after standard.sh. Every tool is optional;
# a failure is logged and skipped so one flaky download never breaks an image.
set -uo pipefail
export DEBIAN_FRONTEND=noninteractive
RUNNER_USER="${TRUNER_USER:-runner}"
ARCH="$(dpkg --print-architecture)"
. /etc/os-release
APT=(apt-get -o DPkg::Lock::Timeout=600 -o Acquire::Retries=5 -yq)
warn() { echo "::truner-warning:: $*"; }
try() { "$@" || warn "failed: $*"; }
aptget() {
  "${APT[@]}" install "$@" && return 0
  warn "bulk install failed, retrying packages one by one"
  local p
  for p in "$@"; do "${APT[@]}" install "$p" >/dev/null 2>&1 || warn "package $p failed"; done
}
addpath() { grep -qxF "$1" /etc/truner/extra-path 2>/dev/null || echo "$1" >>/etc/truner/extra-path; }
addenv() { grep -q "^$1=" /etc/truner/extra-env 2>/dev/null || echo "$1=$2" >>/etc/truner/extra-env; }
case "$ARCH" in amd64) NARCH=x64; JARCH=amd64 ;; arm64) NARCH=arm64; JARCH=arm64 ;; esac

# HashiCorp repository (terraform, packer)
if curl -fsSL --retry 5 https://apt.releases.hashicorp.com/gpg | gpg --dearmor -o /etc/apt/keyrings/hashicorp.gpg --yes; then
  echo "deb [arch=${ARCH} signed-by=/etc/apt/keyrings/hashicorp.gpg] https://apt.releases.hashicorp.com ${VERSION_CODENAME} main" >/etc/apt/sources.list.d/hashicorp.list
fi
"${APT[@]}" update || true

aptget openjdk-17-jdk-headless openjdk-21-jdk-headless maven ant \
  ruby-full clang lld llvm cmake ninja-build meson autoconf automake libtool \
  shellcheck sqlite3 libsqlite3-dev postgresql-client default-mysql-client redis-tools \
  pipx python3-dev python-is-python3 \
  imagemagick ffmpeg jq yamllint \
  libyaml-dev libreadline-dev zlib1g-dev libbz2-dev liblzma-dev libncurses-dev \
  terraform packer

try "${APT[@]}" install dotnet-sdk-8.0

for v in 17 21; do
  if [ -d "/usr/lib/jvm/java-${v}-openjdk-${JARCH}" ]; then
    addenv "JAVA_HOME_${v}_X64" "/usr/lib/jvm/java-${v}-openjdk-${JARCH}"
    [ "$ARCH" = arm64 ] && addenv "JAVA_HOME_${v}_ARM64" "/usr/lib/jvm/java-${v}-openjdk-${JARCH}"
  fi
done
[ -d "/usr/lib/jvm/java-17-openjdk-${JARCH}" ] && addenv JAVA_HOME "/usr/lib/jvm/java-17-openjdk-${JARCH}"

# Node.js LTS (system-wide) with corepack (yarn, pnpm)
if ! command -v node >/dev/null 2>&1; then
  NODE_VER="$(curl -fsSL --retry 5 https://nodejs.org/dist/index.json | jq -r '[.[] | select(.lts != false)][0].version')"
  if [ -n "$NODE_VER" ] && curl -fsSL --retry 5 "https://nodejs.org/dist/${NODE_VER}/node-${NODE_VER}-linux-${NARCH}.tar.xz" -o /tmp/node.tar.xz; then
    tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1 && rm -f /tmp/node.tar.xz
    command -v corepack >/dev/null 2>&1 && corepack enable >/dev/null 2>&1 || true
    try npm install -g npm@latest yarn pnpm --no-fund --no-audit
  else
    warn "node install failed"
  fi
fi

# Go
if [ ! -x /usr/local/go/bin/go ]; then
  GO_VER="$(curl -fsSL --retry 5 'https://go.dev/VERSION?m=text' | head -n1)"
  if [ -n "$GO_VER" ] && curl -fsSL --retry 5 "https://go.dev/dl/${GO_VER}.linux-${ARCH}.tar.gz" -o /tmp/go.tgz; then
    tar -xzf /tmp/go.tgz -C /usr/local && rm -f /tmp/go.tgz
  else
    warn "go install failed"
  fi
fi
addpath /usr/local/go/bin
addpath "/home/${RUNNER_USER}/go/bin"

# Rust (system-wide, writable by the runner user like on GitHub-hosted runners)
export RUSTUP_HOME=/usr/share/rust/.rustup CARGO_HOME=/usr/share/rust/.cargo
if [ ! -x "$CARGO_HOME/bin/cargo" ]; then
  if curl -fsSL --retry 5 https://sh.rustup.rs -o /tmp/rustup.sh; then
    try sh /tmp/rustup.sh -y --default-toolchain stable --profile minimal --component rustfmt,clippy --no-modify-path
    rm -f /tmp/rustup.sh
  fi
fi
chown -R "$RUNNER_USER:$RUNNER_USER" /usr/share/rust 2>/dev/null || true
addenv RUSTUP_HOME /usr/share/rust/.rustup
addenv CARGO_HOME /usr/share/rust/.cargo
addpath /usr/share/rust/.cargo/bin

# Kubernetes tooling
if ! command -v kubectl >/dev/null 2>&1; then
  KV="$(curl -fsSL --retry 5 https://dl.k8s.io/release/stable.txt)"
  try curl -fsSL --retry 5 "https://dl.k8s.io/release/${KV}/bin/linux/${ARCH}/kubectl" -o /usr/local/bin/kubectl
  chmod 0755 /usr/local/bin/kubectl 2>/dev/null || true
fi
if ! command -v helm >/dev/null 2>&1; then
  curl -fsSL --retry 5 https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash >/dev/null 2>&1 || warn "helm install failed"
fi
if ! command -v kind >/dev/null 2>&1; then
  try curl -fsSL --retry 5 "https://kind.sigs.k8s.io/dl/latest/kind-linux-${ARCH}" -o /usr/local/bin/kind
  chmod 0755 /usr/local/bin/kind 2>/dev/null || true
fi

# pipx tools
export PIPX_HOME=/opt/pipx PIPX_BIN_DIR=/opt/pipx_bin
mkdir -p "$PIPX_HOME" "$PIPX_BIN_DIR"
try pipx install poetry
chmod -R a+rX /opt/pipx /opt/pipx_bin 2>/dev/null || true
addenv PIPX_HOME /opt/pipx
addenv PIPX_BIN_DIR /opt/pipx_bin
addpath /opt/pipx_bin

echo "full profile done"
