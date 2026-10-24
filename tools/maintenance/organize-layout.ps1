param(
    [string]$Root = (Join-Path $PSScriptRoot '../..'),
    [string]$Plan = (Join-Path $PSScriptRoot 'layout-2026-09-19.json'),
    [switch]$Apply
)

# Default is inventory only. Never removes files and never follows reparse points.
$ErrorActionPreference = 'Stop'
$rootPath = (Resolve-Path -LiteralPath $Root).Path
$prefix = $rootPath.TrimEnd([IO.Path]::DirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
function In-Root([string]$Relative) {
    if ([IO.Path]::IsPathRooted($Relative)) { throw "Absolute paths are forbidden: $Relative" }
    $full = [IO.Path]::GetFullPath((Join-Path $rootPath $Relative))
    if (-not $full.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) { throw "Outside worktree: $Relative" }
    return $full
}
function Relative-ToRoot([string]$Full) { return [IO.Path]::GetRelativePath($rootPath, $Full).Replace('\', '/') }
$moves = @(Get-Content -LiteralPath $Plan -Raw | ConvertFrom-Json)
$inventory = @()
foreach ($move in $moves) {
    $source = In-Root $move.from
    $target = In-Root $move.to
    if (-not (Test-Path -LiteralPath $source)) { throw "Missing source: $source" }
    if (Test-Path -LiteralPath $target) { throw "Destination already exists: $target" }
    if ($target.StartsWith($source + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) { throw 'Cannot move a directory into itself' }
    $entry = Get-Item -LiteralPath $source -Force
    $children = if ($entry.PSIsContainer) { @(Get-ChildItem -LiteralPath $source -Recurse -Force) } else { @() }
    foreach ($item in @($entry) + $children) {
        if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw "Refusing link/junction: $($item.FullName)" }
    }
    $files = if ($entry.PSIsContainer) { @($children | Where-Object { -not $_.PSIsContainer }) } else { @($entry) }
    foreach ($file in $files) {
        $destination = if ($entry.PSIsContainer) { Join-Path $target ([IO.Path]::GetRelativePath($source, $file.FullName)) } else { $target }
        $inventory += [PSCustomObject]@{ from = (Relative-ToRoot $file.FullName); to = (Relative-ToRoot $destination); bytes = $file.Length; sha256 = (Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash.ToLowerInvariant() }
    }
}
Write-Host ("Plan: {0} moves, {1} files, {2:N2} MiB; no deletions." -f $moves.Count, $inventory.Count, (($inventory | Measure-Object bytes -Sum).Sum / 1MB))
if (-not $Apply) { $moves | Format-Table from,to; return }

function Map-Path([string]$Path) {
    foreach ($move in $moves) {
        $source = In-Root $move.from
        if ($Path.Equals($source, [StringComparison]::OrdinalIgnoreCase)) { return (In-Root $move.to) }
        if ($Path.StartsWith($source + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
            return (Join-Path (In-Root $move.to) ([IO.Path]::GetRelativePath($source, $Path)))
        }
    }
    return $Path
}

# Remember old Markdown locations before moving, to rebase relative links correctly.
$documents = @()
foreach ($relative in @('README.md','manager/README.md','manager/web/README.md','USER_MANUAL.md','REFACTORING.md','manager/web/设备版本管理说明.md')) {
    $path = In-Root $relative
    if (Test-Path -LiteralPath $path) { $documents += $path }
}
foreach ($relative in @('docs','manager/docs')) {
    $path = In-Root $relative
    if (Test-Path -LiteralPath $path) { $documents += @(Get-ChildItem -LiteralPath $path -Recurse -File -Filter '*.md' | ForEach-Object FullName) }
}
$ledger = In-Root ('artifacts/maintenance/layout-' + (Get-Date -Format 'yyyyMMdd-HHmmss') + '.json')
New-Item -ItemType Directory -Path (Split-Path -Parent $ledger) -Force | Out-Null
$record = @{ root = $rootPath; completed = $false; moves = $moves; files = $inventory }
$record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $ledger -Encoding utf8
foreach ($move in $moves) {
    $source = In-Root $move.from
    $target = In-Root $move.to
    New-Item -ItemType Directory -Path (Split-Path -Parent $target) -Force | Out-Null
    Move-Item -LiteralPath $source -Destination $target
}
foreach ($file in $inventory) {
    if ((Get-FileHash -LiteralPath (In-Root $file.to) -Algorithm SHA256).Hash.ToLowerInvariant() -ne $file.sha256) { throw "Content verification failed: $($file.to)" }
}
foreach ($old in ($documents | Select-Object -Unique)) {
    $new = Map-Path $old
    $content = [IO.File]::ReadAllText($new)
    $rebased = [regex]::Replace($content, '\]\((?<link>[^)\r\n]+)\)', {
        param($match)
        $link = $match.Groups['link'].Value
        if ($link -match '^(?:[a-zA-Z][a-zA-Z0-9+.-]*:|#|/)') { return $match.Value }
        $parts = $link -split '(?=[#?])', 2
        $pathPart = $parts[0].Trim('<','>')
        $resolved = [IO.Path]::GetFullPath((Join-Path (Split-Path -Parent $old) ([Uri]::UnescapeDataString($pathPart))))
        $mapped = Map-Path $resolved
        $relative = [IO.Path]::GetRelativePath((Split-Path -Parent $new), $mapped).Replace('\','/')
        $suffix = if ($parts.Count -gt 1) { $parts[1] } else { '' }
        return '](' + $relative + $suffix + ')'
    })
    if ($rebased -cne $content) { [IO.File]::WriteAllText($new, $rebased, [Text.UTF8Encoding]::new($false)) }
}
$record.completed = $true
$record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $ledger -Encoding utf8
Write-Host "Moved and hash-verified all files, then rebased Markdown links. Ledger: $ledger"
