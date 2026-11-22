# Installs Denco on Windows (DECISIONS D67). In PowerShell:
#
#   irm https://raw.githubusercontent.com/7Eshaan/Denco/main/scripts/install.ps1 | iex
#
# It downloads the latest release for this CPU from GitHub, checks it
# against the release's SHA256SUMS, and runs `denco setup`, which installs
# it to %LocalAppData%\Programs\Denco and puts that on your PATH (no admin
# rights). Read it first if you like: it does nothing else.
#
# $env:DENCO_VERSION = "v0.3.0" picks a version instead of the latest.
# ($env:DENCO_REPO_URL points elsewhere; the tests use it.)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # Invoke-WebRequest is far slower with the progress bar

& {
    $repo = if ($env:DENCO_REPO_URL) { $env:DENCO_REPO_URL } else { 'https://github.com/7Eshaan/Denco' }

    function Fail([string]$msg) { throw "denco install: $msg" }

    # Windows PowerShell 5.1 may default to TLS 1.0, which GitHub refuses.
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    # The machine's CPU, also from 32-bit PowerShell on 64-bit Windows.
    $cpu = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    $arch = switch ($cpu) {
        'AMD64' { 'amd64' }
        'ARM64' { 'arm64' }
        default { Fail "no prebuilt Denco for $cpu CPUs; build it from source (see the README)" }
    }

    # The latest version: /releases/latest redirects to /releases/tag/<tag>.
    # (No GitHub API: it limits requests per address.)
    $version = $env:DENCO_VERSION
    if (-not $version) {
        $req = [Net.HttpWebRequest]::Create("$repo/releases/latest")
        $req.AllowAutoRedirect = $false
        $resp = $req.GetResponse()
        $location = $resp.Headers['Location']
        $resp.Close()
        if (-not $location) { Fail "couldn't find the latest release at $repo" }
        $version = $location.TrimEnd('/').Split('/')[-1]
        if ($version -notmatch '^v[0-9]') { Fail "couldn't find the latest release (got $location)" }
    }

    $name = "denco_${version}_windows_${arch}"
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("denco-install-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        Write-Host "Downloading Denco $version for windows/$arch..."
        $zip = Join-Path $tmp "$name.zip"
        $sums = Join-Path $tmp 'SHA256SUMS'
        Invoke-WebRequest -UseBasicParsing -Uri "$repo/releases/download/$version/$name.zip" -OutFile $zip
        Invoke-WebRequest -UseBasicParsing -Uri "$repo/releases/download/$version/SHA256SUMS" -OutFile $sums

        $want = $null
        foreach ($line in Get-Content $sums) {
            $f = $line -split '\s+', 2
            if ($f.Count -eq 2 -and $f[1].TrimStart('*') -eq "$name.zip") { $want = $f[0].ToLower() }
        }
        if (-not $want) { Fail "$name.zip isn't listed in SHA256SUMS" }
        $got = (Get-FileHash -Algorithm SHA256 -Path $zip).Hash.ToLower()
        if ($got -ne $want) { Fail "checksum mismatch for $name.zip (expected $want, got $got): not installing" }
        Write-Host 'Checksum OK.'

        Expand-Archive -Path $zip -DestinationPath $tmp -Force
        $exe = Join-Path (Join-Path $tmp $name) 'denco.exe'
        if (-not (Test-Path $exe)) { Fail "no denco.exe in $name.zip" }

        # denco setup copies it to %LocalAppData%\Programs\Denco and adds
        # that to the user PATH (for terminals opened from now on).
        & $exe setup
        if ($LASTEXITCODE -ne 0) { Fail "denco setup failed (Denco $version may be older than 'denco setup': use v0.3.0 or newer)" }

        # This PowerShell window too, so 'denco' works right away.
        $dir = Join-Path $env:LOCALAPPDATA 'Programs\Denco'
        if (($env:Path -split ';') -notcontains $dir) { $env:Path = "$env:Path;$dir" }
    }
    finally {
        Remove-Item -Recurse -Force -Path $tmp -ErrorAction SilentlyContinue
    }
}
