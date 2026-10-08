[CmdletBinding()]
param(
    [string]$Root = 'S:\OpenPost',
    [string]$RepoUrl = 'https://github.com/siboUNce/openpost.git',
    [string]$Revision = 'e031fba1531f4180c840a3a996c9a5ed1a035f05',
    [string]$Image = 'saivaree/openpost:current-main-e031fba',
    [string]$EnvBackupPath = '',
    [string]$DatabaseBackupPath = ''
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$packageRoot = $PSScriptRoot
$composeTemplate = Join-Path $packageRoot 'docker-compose.yml'
$envExample = Join-Path $packageRoot '.env.example'
$dockerfile = Join-Path $packageRoot 'Dockerfile'
$composePath = Join-Path $Root 'docker-compose.yml'
$envPath = Join-Path $Root '.env'
$dataRoot = Join-Path $Root 'data'
$dbRoot = Join-Path $dataRoot 'db'
$mediaRoot = Join-Path $dataRoot 'media'
$buildRoot = Join-Path $Root 'build'
$sourceRoot = Join-Path $buildRoot ('source-' + $Revision.Substring(0, 12))
$rollbackRoot = Join-Path $Root 'deployment-rollbacks'
$privateOrigin = 'http://100.67.146.88:4407'
$publicOrigin = 'https://post.saivareeclinic.com'
$cloudflaredRoot = 'C:\ProgramData\Saivaree\cloudflared'
$cloudflaredConfig = Join-Path $cloudflaredRoot 'config-docker.yml'
$cloudflaredCredential = Join-Path $cloudflaredRoot 'tunnel.json'
$stamp = Get-Date -Format 'yyyyMMddTHHmmss'
$checkpoint = Join-Path $rollbackRoot ('restore-' + $stamp)
$hadExistingRuntime = Test-Path $composePath

function Assert-Native([string]$Action) {
    if ($LASTEXITCODE -ne 0) {
        throw "$Action failed (exit $LASTEXITCODE)."
    }
}

function Get-EnvMap([string]$Path) {
    $map = @{}
    foreach ($line in Get-Content -LiteralPath $Path) {
        $trimmed = $line.Trim()
        if ($trimmed -eq '' -or $trimmed.StartsWith('#') -or -not $trimmed.Contains('=')) {
            continue
        }
        $parts = $trimmed.Split('=', 2)
        $map[$parts[0].Trim()] = $parts[1]
    }
    return $map
}

function Set-EnvValue([string]$Path, [string]$Name, [string]$Value) {
    $lines = @(Get-Content -LiteralPath $Path)
    $found = $false
    for ($i = 0; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -match ('^\s*' + [regex]::Escape($Name) + '=')) {
            $lines[$i] = "$Name=$Value"
            $found = $true
        }
    }
    if (-not $found) {
        $lines += "$Name=$Value"
    }
    [IO.File]::WriteAllLines($Path, $lines, [Text.UTF8Encoding]::new($false))
}

function Wait-Ready([string]$Origin, [int]$Seconds = 120) {
    $deadline = (Get-Date).AddSeconds($Seconds)
    do {
        try {
            $ready = Invoke-RestMethod "$Origin/api/v1/ready" -TimeoutSec 5
            if ($ready.status -eq 'ready' -and $ready.database -eq 'ok') {
                return $true
            }
        } catch {}
        Start-Sleep -Seconds 2
    } while ((Get-Date) -lt $deadline)
    return $false
}

function Wait-Revision([string]$Origin, [string]$ExpectedRevision, [int]$Seconds = 120) {
    $deadline = (Get-Date).AddSeconds($Seconds)
    do {
        try {
            $version = Invoke-RestMethod "$Origin/api/v1/version" -TimeoutSec 5
            if ($version.revision -eq $ExpectedRevision) {
                return $version
            }
        } catch {}
        Start-Sleep -Seconds 2
    } while ((Get-Date) -lt $deadline)
    throw "Revision verification timed out at $Origin."
}

function Backup-ExistingRuntime {
    if (-not $hadExistingRuntime) {
        return
    }
    New-Item -ItemType Directory -Path $checkpoint -Force | Out-Null
    Copy-Item $composePath (Join-Path $checkpoint 'docker-compose.yml') -Force
    if (Test-Path $envPath) {
        Copy-Item $envPath (Join-Path $checkpoint '.env') -Force
    }
    $running = docker ps --format '{{.Names}}' | Where-Object { $_ -eq 'openpost-smoke' }
    if ($running) {
        docker inspect openpost-smoke | Set-Content -LiteralPath (Join-Path $checkpoint 'openpost-smoke.inspect.json')
        Assert-Native 'Inspect existing OpenPost container'
        $temp = "/data/db/restore-preflight-$stamp.db"
        docker exec openpost-smoke sqlite3 /data/db/openpost.db ".backup '$temp'"
        Assert-Native 'Create online SQLite backup'
        docker cp "openpost-smoke:$temp" (Join-Path $checkpoint 'openpost.db')
        Assert-Native 'Copy SQLite backup'
        docker exec openpost-smoke rm -- $temp | Out-Null
    } elseif (Test-Path (Join-Path $dbRoot 'openpost.db')) {
        Copy-Item (Join-Path $dbRoot 'openpost.db') (Join-Path $checkpoint 'openpost.db') -Force
    }
}

function Restore-PreviousRuntime {
    if (-not $hadExistingRuntime -or -not (Test-Path $checkpoint)) {
        return $false
    }
    try {
        if (Test-Path (Join-Path $checkpoint 'docker-compose.yml')) {
            Copy-Item (Join-Path $checkpoint 'docker-compose.yml') $composePath -Force
        }
        if (Test-Path (Join-Path $checkpoint '.env')) {
            Copy-Item (Join-Path $checkpoint '.env') $envPath -Force
        }
        if (Test-Path (Join-Path $checkpoint 'openpost.db')) {
            docker compose -f $composePath --env-file $envPath stop openpost | Out-Null
            Copy-Item (Join-Path $checkpoint 'openpost.db') (Join-Path $dbRoot 'openpost.db') -Force
            Remove-Item (Join-Path $dbRoot 'openpost.db-wal'), (Join-Path $dbRoot 'openpost.db-shm') -ErrorAction SilentlyContinue
        }
        Set-Location $Root
        docker compose --env-file $envPath -f $composePath up -d
        return $true
    } catch {
        Write-Warning ("Automatic rollback failed: " + $_.Exception.Message)
        return $false
    }
}

if ((hostname).Trim() -ine 'Clinic_Server') {
    throw 'Wrong host. Run this script on CLINIC_SERVER only.'
}

foreach ($command in @('docker', 'git')) {
    if (-not (Get-Command $command -ErrorAction SilentlyContinue)) {
        throw "$command is required but was not found."
    }
}

New-Item -ItemType Directory -Path $Root, $dataRoot, $dbRoot, $mediaRoot, $buildRoot, $rollbackRoot -Force | Out-Null
Backup-ExistingRuntime

try {
    if (-not (Test-Path $envPath)) {
        if ($EnvBackupPath -and (Test-Path $EnvBackupPath)) {
            Copy-Item $EnvBackupPath $envPath -Force
        } else {
            Copy-Item $envExample $envPath -Force
            throw "No production .env was found. A safe template was created at $envPath. Restore the original secrets or pass -EnvBackupPath, then rerun."
        }
    }

    $envMap = Get-EnvMap $envPath
    foreach ($required in @('OPENPOST_APP_URL','OPENPOST_PUBLIC_URL','OPENPOST_MEDIA_URL','OPENPOST_JWT_SECRET','OPENPOST_ENCRYPTION_KEY')) {
        if (-not $envMap.ContainsKey($required) -or [string]::IsNullOrWhiteSpace($envMap[$required]) -or $envMap[$required] -like 'REPLACE_*') {
            throw "Required environment value $required is missing or still a placeholder."
        }
    }

    Set-EnvValue $envPath 'OPENPOST_DISABLE_REGISTRATIONS' 'true'
    Set-EnvValue $envPath 'OPENPOST_DISABLE_TIKTOK_DISPLAY_API' 'true'
    Set-EnvValue $envPath 'OPENPOST_IMAGE' $Image

    if (-not (Test-Path $cloudflaredConfig)) {
        throw "Missing Cloudflare tunnel config: $cloudflaredConfig"
    }
    if (-not (Test-Path $cloudflaredCredential)) {
        $candidates = @(Get-ChildItem $cloudflaredRoot -Filter '*.json' -File -ErrorAction SilentlyContinue | Where-Object { $_.Name -ne 'tunnel.json' })
        if ($candidates.Count -eq 1) {
            Copy-Item $candidates[0].FullName $cloudflaredCredential -Force
        } else {
            throw "Missing $cloudflaredCredential. Restore the Cloudflare tunnel credential file before continuing."
        }
    }

    if (Test-Path $sourceRoot) {
        Remove-Item $sourceRoot -Recurse -Force
    }
    git clone --no-checkout $RepoUrl $sourceRoot
    Assert-Native 'Clone OpenPost source'
    git -C $sourceRoot fetch --depth 1 origin $Revision
    Assert-Native 'Fetch pinned revision'
    git -C $sourceRoot checkout --detach FETCH_HEAD
    Assert-Native 'Checkout pinned revision'
    $head = (git -C $sourceRoot rev-parse HEAD).Trim()
    Assert-Native 'Read source revision'
    if ($head -ne $Revision) {
        throw "Source revision mismatch: expected $Revision, got $head."
    }

    docker buildx build --load --platform linux/amd64 -f $dockerfile -t $Image --build-arg "VERSION=saivaree-$($Revision.Substring(0,12))" --build-arg "COMMIT=$Revision" $sourceRoot
    Assert-Native 'Build pinned OpenPost image'

    $imageRevision = (docker image inspect $Image --format '{{index .Config.Labels "org.opencontainers.image.revision"}}').Trim()
    Assert-Native 'Inspect built image'
    if ($imageRevision -ne $Revision) {
        throw "Built image revision mismatch: expected $Revision, got $imageRevision."
    }

    if ($DatabaseBackupPath) {
        if (-not (Test-Path $DatabaseBackupPath)) {
            throw "Database backup not found: $DatabaseBackupPath"
        }
        Copy-Item $DatabaseBackupPath (Join-Path $dbRoot 'openpost.db') -Force
        Remove-Item (Join-Path $dbRoot 'openpost.db-wal'), (Join-Path $dbRoot 'openpost.db-shm') -ErrorAction SilentlyContinue
    }

    if (Test-Path (Join-Path $dbRoot 'openpost.db')) {
        $integrity = docker run --rm -v ($dbRoot + ':/data/db') $Image sqlite3 /data/db/openpost.db 'PRAGMA integrity_check;'
        Assert-Native 'Validate SQLite database'
        if (($integrity -join '').Trim() -ne 'ok') {
            throw 'SQLite integrity check failed.'
        }
    }

    Copy-Item $composeTemplate $composePath -Force
    Set-Location $Root
    docker compose --env-file $envPath -f $composePath config --quiet
    Assert-Native 'Validate Docker Compose configuration'
    docker compose --env-file $envPath -f $composePath up -d
    Assert-Native 'Start OpenPost stack'

    if (-not (Wait-Ready $privateOrigin 120)) {
        throw 'Private readiness check failed.'
    }
    $privateVersion = Wait-Revision $privateOrigin $Revision 120
    if (-not (Wait-Ready $publicOrigin 120)) {
        throw 'Public readiness check failed.'
    }
    $publicVersion = Wait-Revision $publicOrigin $Revision 120

    $metadata = Invoke-RestMethod "$publicOrigin/.well-known/oauth-authorization-server" -TimeoutSec 10
    if ($metadata.token_endpoint_auth_methods_supported -notcontains 'none') {
        throw 'ChatGPT MCP OAuth compatibility check failed: token auth method "none" is missing.'
    }

    $tiktokFlag = docker exec openpost-smoke sh -lc 'printf %s "$OPENPOST_DISABLE_TIKTOK_DISPLAY_API"'
    Assert-Native 'Verify TikTok scope flag'
    if ($tiktokFlag.Trim().ToLowerInvariant() -ne 'true') {
        throw 'TikTok Display API disable flag was not loaded into the container.'
    }

    [ordered]@{
        status = 'PASS'
        revision = $privateVersion.revision
        public_revision = $publicVersion.revision
        image = $Image
        private_ready = $true
        public_ready = $true
        oauth_public_client = $true
        tiktok_minimal_scopes = $true
        checkpoint = if ($hadExistingRuntime) { $checkpoint } else { $null }
    } | ConvertTo-Json -Depth 4
} catch {
    $reason = $_.Exception.Message
    $rolledBack = Restore-PreviousRuntime
    [ordered]@{
        status = 'FAIL'
        reason = $reason
        rollback_attempted = $hadExistingRuntime
        rollback_started = $rolledBack
        checkpoint = if ($hadExistingRuntime) { $checkpoint } else { $null }
    } | ConvertTo-Json -Depth 4
    exit 1
}
