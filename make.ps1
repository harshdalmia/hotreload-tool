#Requires -Version 5.1
<#
.SYNOPSIS
    Task runner for hotreload on Windows, mirroring the Makefile.

.DESCRIPTION
    The Makefile needs GNU make and a POSIX shell, neither of which ships with
    Windows. This script exposes the same tasks natively so `.\make.ps1 test`
    works from a plain PowerShell prompt.

.PARAMETER Task
    The task to run. Omit it or pass "help" to list the available tasks.

.PARAMETER Version
    Version string stamped into the binary. Defaults to "dev".

.EXAMPLE
    .\make.ps1 build

.EXAMPLE
    .\make.ps1 demo

.EXAMPLE
    .\make.ps1 build -Version 1.2.3
#>
[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [string]$Task = 'help',

    [string]$Version = 'dev',
    [string]$Commit = 'none',
    [string]$Date = 'unknown'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$RootDir       = $PSScriptRoot
$BinDir        = Join-Path $RootDir 'bin'
$BinaryPath    = Join-Path $BinDir 'hotreload.exe'
$TestServerBin = Join-Path $BinDir 'testserver.exe'
$MainPkg       = './cmd/hotreload'
$TestServerDir = Join-Path $RootDir 'testserver'
$LdFlags       = "-X main.version=$Version -X main.commit=$Commit -X main.date=$Date"

# Invoke an external command and stop the script if it reports failure.
# PowerShell does not do this by default for native executables.
function Invoke-Checked {
    param([Parameter(Mandatory)][string]$Exe, [string[]]$Arguments = @())

    Write-Host "> $Exe $($Arguments -join ' ')" -ForegroundColor DarkGray
    & $Exe @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "$Exe exited with code $LASTEXITCODE"
    }
}

function Invoke-Go {
    param([Parameter(ValueFromRemainingArguments)][string[]]$Arguments)
    Invoke-Checked -Exe 'go' -Arguments $Arguments
}

function Task-Help {
    Write-Host 'hotreload tasks (.\make.ps1 <task>):' -ForegroundColor Cyan
    @(
        @{ n = 'build';         d = 'Build .\bin\hotreload.exe' }
        @{ n = 'install';       d = 'go install the CLI' }
        @{ n = 'demo';          d = 'Build and run the demo against .\testserver' }
        @{ n = 'demo-verbose';  d = 'Same as demo, with debug logging' }
        @{ n = 'test';          d = 'Run all tests' }
        @{ n = 'test-short';    d = 'Run tests, skipping the slow ones' }
        @{ n = 'test-e2e';      d = 'Run only the end-to-end tests' }
        @{ n = 'test-race';     d = 'Run tests under the race detector (needs 64-bit gcc)' }
        @{ n = 'test-coverage'; d = 'Write coverage.out and coverage.html' }
        @{ n = 'fmt';           d = 'Format the tree with gofmt' }
        @{ n = 'fmt-check';     d = 'Fail if anything is unformatted' }
        @{ n = 'vet';           d = 'Run go vet' }
        @{ n = 'lint';          d = 'Run golangci-lint' }
        @{ n = 'lint-install';  d = 'Install the pinned golangci-lint version' }
        @{ n = 'check';         d = 'fmt-check + vet + test' }
        @{ n = 'ci';            d = 'fmt-check + vet + test-race' }
        @{ n = 'deps';          d = 'Tidy and download dependencies' }
        @{ n = 'clean';         d = 'Remove build artefacts' }
    ) | ForEach-Object { Write-Host ('  {0,-14} {1}' -f $_.n, $_.d) }
}

function Task-Build {
    Invoke-Go build '-ldflags' $LdFlags '-o' $BinaryPath $MainPkg
    Write-Host "Built $BinaryPath" -ForegroundColor Green
}

function Task-Install {
    Invoke-Go install '-ldflags' $LdFlags $MainPkg
}

function Task-Demo {
    # Not named -Verbose: that is a PowerShell common parameter and shadowing
    # it here would be confusing.
    param([switch]$DebugLogging)

    Task-Build

    Write-Host ''
    Write-Host '==========================================' -ForegroundColor Cyan
    Write-Host '  hotreload demo'
    Write-Host '  Edit testserver\main.go and save.'
    Write-Host '  Visit http://localhost:8080'
    Write-Host '==========================================' -ForegroundColor Cyan
    Write-Host ''

    # Not $args: that is an automatic variable in PowerShell.
    $cliArgs = @(
        '--root', $TestServerDir
        '--build', "go build -C `"$TestServerDir`" -o `"$TestServerBin`" ."
        '--exec', "`"$TestServerBin`""
        '--include-ext', '.go'
    )
    if ($DebugLogging) { $cliArgs += '--verbose' }

    # Ctrl+C is the intended way to stop the demo, so a non-zero exit here is
    # expected and should not be reported as a task failure.
    & $BinaryPath @cliArgs
}

function Task-Test        { Invoke-Go test './...' '-count=1' }
function Task-TestShort   { Invoke-Go test '-short' './...' '-count=1' }
function Task-TestE2E     { Invoke-Go test './e2e/' '-count=1' '-v' '-timeout' '15m' }
function Task-TestRace    { Invoke-Go test '-race' './...' '-count=1' '-timeout' '20m' }

function Task-TestCoverage {
    Invoke-Go test './...' '-coverprofile=coverage.out' '-covermode=atomic'
    Invoke-Go tool cover '-html=coverage.out' '-o' 'coverage.html'
    Write-Host 'Coverage report written to coverage.html' -ForegroundColor Green
}

function Task-Fmt { Invoke-Checked -Exe 'gofmt' -Arguments @('-w', '.') }

function Task-FmtCheck {
    $unformatted = & gofmt -l . | Where-Object { $_ -ne '' }
    if ($unformatted) {
        Write-Host 'These files are not gofmt-clean:' -ForegroundColor Red
        $unformatted | ForEach-Object { Write-Host "  $_" }
        throw "Run '.\make.ps1 fmt' to fix them."
    }
    Write-Host 'gofmt: clean' -ForegroundColor Green
}

function Task-Vet { Invoke-Go vet './...' }

# Keep in step with the version pinned in .github/workflows/ci.yml.
$GolangciVersion = 'v2.8.0'

function Task-Lint {
    if (-not (Get-Command golangci-lint -ErrorAction SilentlyContinue)) {
        # The v2 module path matters: the old path installs a v1 binary, which
        # cannot read the v2 .golangci.yml and fails with a schema error.
        throw ".\make.ps1 lint-install first (golangci-lint not found on PATH)"
    }
    Invoke-Checked -Exe 'golangci-lint' -Arguments @('run')
}

function Task-LintInstall {
    Invoke-Go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$GolangciVersion"
}

function Task-Deps {
    Invoke-Go mod tidy
    Invoke-Go mod download
}

function Task-Clean {
    foreach ($path in @($BinDir, (Join-Path $RootDir 'coverage.out'), (Join-Path $RootDir 'coverage.html'))) {
        if (Test-Path $path) {
            Remove-Item -Recurse -Force $path
            Write-Host "Removed $path" -ForegroundColor DarkGray
        }
    }
}

switch ($Task.ToLowerInvariant()) {
    'help'          { Task-Help }
    'build'         { Task-Build }
    'install'       { Task-Install }
    'demo'          { Task-Demo }
    'demo-verbose'  { Task-Demo -DebugLogging }
    'test'          { Task-Test }
    'test-short'    { Task-TestShort }
    'test-e2e'      { Task-TestE2E }
    'test-race'     { Task-TestRace }
    'test-coverage' { Task-TestCoverage }
    'fmt'           { Task-Fmt }
    'fmt-check'     { Task-FmtCheck }
    'vet'           { Task-Vet }
    'lint'          { Task-Lint }
    'lint-install'  { Task-LintInstall }
    'check'         { Task-FmtCheck; Task-Vet; Task-Test }
    'ci'            { Task-FmtCheck; Task-Vet; Task-TestRace }
    'deps'          { Task-Deps }
    'clean'         { Task-Clean }
    default {
        Write-Host "Unknown task '$Task'." -ForegroundColor Red
        Task-Help
        exit 1
    }
}
