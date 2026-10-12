# she in PowerShell, for people typing (Claude Code sends stdin and needs none
# of this). Read from the profile: . "$HOME\.config\she\completion.ps1"
# (she --powershell-completion writes this file).
#
#   she [NAME] <  then Tab    '|' | she [NAME]            (| is the cursor)
#   Tab / Shift+Tab again     @' / | / '@ | she [NAME]    and back
#   in the quotes or on a     Tab completes from the server's bash (in NAME's
#     line of @' '@           directory): a lone completion or the common start
#                             of several goes in; else Tab / Shift+Tab go
#                             through them, as PowerShell's Tab does
#   she <Tab>, she pw<Tab>    the names of the kept bashes and the options
#   she NAME alone            reads a line without showing it (a password)
#                             and sends it to the command running in NAME
#
# In @' ... '@ Shift+Enter adds a line and Enter runs it (PSReadLine's own
# keys). Any other Tab and Shift+Tab work as before.

function __SheExe {
    $e = (Get-Command she.exe -ErrorAction SilentlyContinue).Source
    if ($e) { $e } else { "$HOME\bin\she.exe" }
}

# the forms for NAME ('' for none): text, and where the cursor goes
function __SheForms([string]$name) {
    $t = if ($name) { " $name" } else { "" }
    , @(@("'' | she$t", 1), @("@'`n`n'@ | she$t", 3))
}

# the text before the cursor inside she's quotes: where it starts, the text
# (with '' as '), the name, and whether it is in '...' (not @' '@); or $null
function __SheInQuote([string]$line, [int]$cursor) {
    $n = '(?: ([^-\s<>|;&][^\s<>|;&]*))?'
    if ($line -match "^'((?:[^']|'')*)' \| she$n$") {
        if ($cursor -ge 1 -and $cursor -le 1 + $Matches[1].Length) {
            return @{ start = 1; text = $line.Substring(1, $cursor - 1).Replace("''", "'"); name = $Matches[2]; q = $true }
        }
    } elseif ($line -match "^@'\n([\s\S]*)\n'@ \| she$n$") {
        if ($cursor -ge 3 -and $cursor -le 3 + $Matches[1].Length) {
            $s = $line.LastIndexOf("`n", $cursor - 1) + 1
            return @{ start = $s; text = $line.Substring($s, $cursor - $s); name = $Matches[2]; q = $false }
        }
    }
    $null
}

# completion from the server inside the quotes; $get NAME TEXT gives the
# completions (each the whole text). Several with no longer common start:
# Tab / Shift+Tab go through them ($global:SheCycle keeps where we are).
function __SheRemote([string]$line, [int]$cursor, [int]$dir, [scriptblock]$get) {
    $c = $global:SheCycle
    if ($c -and $c.line -ceq $line -and $c.cursor -eq $cursor) {
        $c.i = ($c.i + $dir + $c.cands.Count) % $c.cands.Count
        $new = $c.head + $c.cands[$c.i]
        $c.line = $new + $c.tail
        $c.cursor = $new.Length
        return @($c.line, $c.cursor)
    }
    $global:SheCycle = $null
    $q = __SheInQuote $line $cursor
    if (-not $q) { return $null }
    $cands = @(& $get $q.name $q.text | Where-Object { $_ -ne '' })
    if ($cands.Count -eq 0) { return @($line, $cursor) }
    if ($q.q) { $cands = @($cands | ForEach-Object { $_.Replace("'", "''") }) }
    $head = $line.Substring(0, $q.start)
    $tail = $line.Substring($cursor)
    $typed = $line.Substring($q.start, $cursor - $q.start)
    if ($cands.Count -eq 1) {
        $new = $cands[0]
    } else {
        $p = $cands[0]
        foreach ($x in $cands) {
            $k = 0
            while ($k -lt $p.Length -and $k -lt $x.Length -and $p[$k] -ceq $x[$k]) { $k++ }
            $p = $p.Substring(0, $k)
        }
        if ($p.Length -gt $typed.Length) {
            $new = $p
        } else {
            $i = if ($dir -gt 0) { 0 } else { $cands.Count - 1 }
            $new = $cands[$i]
            $global:SheCycle = @{ cands = $cands; i = $i; head = $head; tail = $tail; line = $head + $new + $tail; cursor = ($head + $new).Length }
        }
    }
    @(($head + $new + $tail), ($head + $new).Length)
}

# the new line and cursor for Tab (1) or Shift+Tab (-1), or $null to leave
# the key to PowerShell
function __SheTab([string]$line, [int]$cursor, [int]$dir, [scriptblock]$get) {
    if ($cursor -eq $line.Length -and $line -match '^\s*she(?:\s+([^-\s<>|;&][^\s<>|;&]*))?\s*<$') {
        $global:SheCycle = $null
        return (__SheForms $Matches[1])[0]
    }
    if ($line -match '^(?:''''|@''\n\n''@) \| she(?: ([^-\s<>|;&][^\s<>|;&]*))?$') {
        $f = __SheForms $Matches[1]
        for ($i = 0; $i -lt 2; $i++) {
            if ($line -ceq $f[$i][0] -and $cursor -eq $f[$i][1]) {
                $global:SheCycle = $null
                return $f[($i + $dir + 2) % 2]
            }
        }
    }
    __SheRemote $line $cursor $dir $get
}

# ask the server: the text on stdin, UTF-8 both ways
$global:SheGet = {
    param($name, $text)
    $exe = __SheExe
    $OutputEncoding = New-Object System.Text.UTF8Encoding $false
    $old = [Console]::OutputEncoding
    try {
        [Console]::OutputEncoding = $OutputEncoding
        if ($name) { $text | & $exe --complete $name 2>$null } else { $text | & $exe --complete 2>$null }
    } finally {
        [Console]::OutputEncoding = $old
    }
}

# Tab on the first argument: the names of the kept bashes (she -l) and the
# options; anything after it is left to PowerShell
Register-ArgumentCompleter -Native -CommandName she, she.exe -ScriptBlock {
    param($word, $ast, $cursor)
    $before = @($ast.CommandElements | Where-Object { $_.Extent.EndOffset -lt $cursor }).Count
    if ($before -ne 1) { return }
    $names = @(& (__SheExe) -l 2>$null | ForEach-Object { ($_ -split "`t")[0] })
    @($names + @('-l', '-start', '-stop', '-setup')) | Where-Object { $_ -like "$word*" } |
        ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
}

# Windows PowerShell 5.1 loads PSReadLine only after the profile: load it here
if ($Host.Name -eq 'ConsoleHost' -and (Import-Module PSReadLine -PassThru -ErrorAction SilentlyContinue)) {
    # what Tab and Shift+Tab did before (PSReadLine 2.0 has no -Chord)
    $bound = Get-PSReadLineKeyHandler -Bound
    $global:SheTabNext = ($bound | Where-Object Key -eq 'Tab' | Select-Object -First 1).Function
    $global:SheTabPrev = ($bound | Where-Object Key -eq 'Shift+Tab' | Select-Object -First 1).Function
    if ($global:SheTabNext -like 'she *') { $global:SheTabNext = 'TabCompleteNext' }
    if ($global:SheTabPrev -like 'she *') { $global:SheTabPrev = 'TabCompletePrevious' }
    foreach ($k in @(@('Tab', 1), @('Shift+Tab', -1))) {
        Set-PSReadLineKeyHandler -Chord $k[0] -BriefDescription "she $($k[0])" -ScriptBlock ([scriptblock]::Create(@"
param(`$key, `$arg)
`$line = `$null; `$cursor = `$null
[Microsoft.PowerShell.PSConsoleReadLine]::GetBufferState([ref]`$line, [ref]`$cursor)
`$r = __SheTab `$line `$cursor $($k[1]) `$global:SheGet
if (`$r) {
    [Microsoft.PowerShell.PSConsoleReadLine]::Replace(0, `$line.Length, `$r[0])
    [Microsoft.PowerShell.PSConsoleReadLine]::SetCursorPosition(`$r[1])
} else {
    `$fn = if ($($k[1]) -gt 0) { `$global:SheTabNext } else { `$global:SheTabPrev }
    if (`$fn) { [Microsoft.PowerShell.PSConsoleReadLine]::`$fn.Invoke(`$key, `$arg) }
}
"@))
    }
}
