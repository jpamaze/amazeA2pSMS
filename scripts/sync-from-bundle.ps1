# Applies a git bundle from a Claude session to the local repo and pushes it.
# Defaults are this machine's paths; override with -RepoPath / -BundlePath.
#
#   powershell -ExecutionPolicy Bypass -File scripts\sync-from-bundle.ps1
#   powershell -ExecutionPolicy Bypass -File scripts\sync-from-bundle.ps1 -NoPush
param(
    [string]$RepoPath   = "D:\ZAPIM\SMPP\amazeA2pSMS",
    [string]$BundlePath = "D:\ZAPIM\SMPP\amazeA2pSMS.bundle",
    [string]$Branch     = "main",
    [switch]$NoPush
)

$ErrorActionPreference = "Stop"

function Invoke-Git {
    git @args
    if ($LASTEXITCODE -ne 0) { throw "git $($args -join ' ') failed (exit $LASTEXITCODE)" }
}

if (-not (Test-Path $BundlePath)) { throw "Bundle not found: $BundlePath" }
if (-not (Test-Path (Join-Path $RepoPath ".git"))) { throw "Not a git repository: $RepoPath" }

Set-Location $RepoPath
Invoke-Git bundle verify $BundlePath

$dirty = git status --porcelain
if ($dirty) { throw "Working tree has uncommitted changes; commit or stash them first." }

# Works on an empty repo (unborn branch) and on one that already has earlier commits.
$current = git symbolic-ref --short HEAD 2>$null
if ($current -ne $Branch) {
    git show-ref --verify --quiet "refs/heads/$Branch"
    if ($LASTEXITCODE -eq 0) { Invoke-Git switch $Branch } else { Invoke-Git switch --orphan $Branch }
}
Invoke-Git pull --ff-only $BundlePath $Branch

Invoke-Git log --oneline -5

if (-not $NoPush) {
    Invoke-Git push -u origin $Branch
}
