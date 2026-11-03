package controlplane

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/codician-team/truner/internal/agentcfg"
	"github.com/codician-team/truner/internal/config"
)

// agentKey is the S3 key of an agent binary in the artifacts bucket.
func agentKey(version, platform, arch string) string {
	goarch := "amd64"
	if arch == config.ArchARM64 {
		goarch = "arm64"
	}
	name := fmt.Sprintf("truner-agent-%s-%s", platform, goarch)
	if platform == config.PlatformWindows {
		name += ".exe"
	}
	return "agent/" + version + "/" + name
}

func (cp *ControlPlane) presign(ctx context.Context, bucket, key string, ttl time.Duration) (string, error) {
	req, err := cp.Presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &key}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

// buildUserData renders the bootstrap script for an instance.
func (cp *ControlPlane) buildUserData(ctx context.Context, c *agentcfg.Config) (string, error) {
	enc, err := agentcfg.Encode(c)
	if err != nil {
		return "", err
	}
	bucket := cp.Env.ArtifactsBucket
	if bucket == "" {
		bucket = cp.Env.Bucket
	}
	agentURL, err := cp.presign(ctx, bucket, agentKey(c.Version, c.Platform, c.Arch), 12*time.Hour)
	if err != nil {
		return "", err
	}
	var ud string
	if c.Platform == config.PlatformWindows {
		ud = windowsUserData(enc, agentURL, c.Version)
	} else {
		ud = linuxUserData(enc, agentURL, c.Version)
	}
	if len(ud) > 16000 {
		return "", fmt.Errorf("user data too large (%d bytes)", len(ud))
	}
	return ud, nil
}

func linuxUserData(enc, agentURL, version string) string {
	return strings.TrimLeft(fmt.Sprintf(`
#!/bin/bash
# truner bootstrap - downloads the agent and hands over control to it.
%s'%s'
mkdir -p /etc/truner /var/log/truner
exec >>/var/log/truner/bootstrap.log 2>&1
echo "truner bootstrap start $(date -Is)"
AGENT=/usr/local/bin/truner-agent
if [ "$("$AGENT" version 2>/dev/null)" != "%s" ]; then
  ok=0
  for i in $(seq 1 20); do
    if curl -fsSL --connect-timeout 5 --max-time 180 -o "$AGENT.tmp" '%s'; then ok=1; break; fi
    sleep 3
  done
  if [ "$ok" != 1 ]; then echo "agent download failed, shutting down"; shutdown -h now; exit 1; fi
  chmod 0755 "$AGENT.tmp" && mv -f "$AGENT.tmp" "$AGENT"
fi
printf '%%s' "$TRUNER_CONFIG" > /etc/truner/config.b64
chmod 0600 /etc/truner/config.b64
if ! systemd-run --unit=truner-agent-run --collect -p LimitNOFILE=1048576 -p TasksMax=infinity "$AGENT" run --config /etc/truner/config.b64; then
  nohup "$AGENT" run --config /etc/truner/config.b64 >/dev/null 2>&1 &
fi
echo "truner bootstrap handed over $(date -Is)"
`, agentcfg.UserDataMarker, enc, version, agentURL), "\n")
}

func windowsUserData(enc, agentURL, version string) string {
	return fmt.Sprintf(`<powershell>
$%s"%s"
$ErrorActionPreference = "Continue"
$ProgressPreference = "SilentlyContinue"
New-Item -ItemType Directory -Force -Path C:\truner | Out-Null
Start-Transcript -Path C:\truner\bootstrap.log -Append | Out-Null
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$agent = "C:\truner\truner-agent.exe"
$current = ""
if (Test-Path $agent) { $current = (& $agent version) 2>$null }
if ($current -ne "%s") {
  $ok = $false
  for ($i = 0; $i -lt 20; $i++) {
    try { Invoke-WebRequest -UseBasicParsing -Uri '%s' -OutFile "$agent.tmp"; $ok = $true; break } catch { Start-Sleep -Seconds 3 }
  }
  if (-not $ok) { Write-Output "agent download failed"; Stop-Computer -Force; exit 1 }
  Move-Item -Force "$agent.tmp" $agent
}
Set-Content -Path C:\truner\config.b64 -Value $TRUNER_CONFIG -NoNewline
Start-Process -FilePath $agent -ArgumentList @("run", "--config", "C:\truner\config.b64") -WindowStyle Hidden
Stop-Transcript | Out-Null
</powershell>
<persist>false</persist>
`, agentcfg.UserDataMarker, enc, version, agentURL)
}

// runnerURLs returns download locations for the runner package, preferring
// the in-region S3 mirror.
func (cp *ControlPlane) runnerURLs(ctx context.Context, rel *runnerRelease, platform, arch string) []string {
	if rel == nil {
		return nil
	}
	file := runnerFile(rel.Version, platform, arch)
	var urls []string
	if rel.Mirrored[file] {
		if u, err := cp.presign(ctx, cp.Env.Bucket, "runners/"+file, 12*time.Hour); err == nil {
			urls = append(urls, u)
		}
	}
	urls = append(urls, fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/%s", rel.Version, file))
	return urls
}

func runnerFile(version, platform, arch string) string {
	if platform == config.PlatformWindows {
		return fmt.Sprintf("actions-runner-win-x64-%s.zip", version)
	}
	a := "x64"
	if arch == config.ArchARM64 {
		a = "arm64"
	}
	return fmt.Sprintf("actions-runner-linux-%s-%s.tar.gz", a, version)
}
