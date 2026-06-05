# teleclaude launcher
# teleclaude.exe가 exit code 42로 종료하면 teleclaude_new.exe로 교체 후 재시작.
# 사용법: .\launcher.ps1

$dir = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $dir

Write-Host "[launcher] teleclaude 시작 ($dir)"

# 기존에 실행 중인 다른 teleclaude 인스턴스 정리
$existing = Get-Process teleclaude -ErrorAction SilentlyContinue |
    Where-Object { $_.Id -ne $PID }
if ($existing) {
    Write-Host "[launcher] 기존 인스턴스 종료 중... ($($existing.Count)개)"
    $existing | Stop-Process -Force
    Start-Sleep -Milliseconds 800
}

while ($true) {
    & ".\teleclaude.exe"
    $code = $LASTEXITCODE

    if ($code -eq 42) {
        Write-Host "[launcher] 업데이트 감지 (exit 42) → 교체 중..."
        if (Test-Path "teleclaude_new.exe") {
            Move-Item -Force "teleclaude_new.exe" "teleclaude.exe"
            Write-Host "[launcher] 교체 완료 → 재시작"
        } else {
            Write-Host "[launcher] teleclaude_new.exe 없음 → 그냥 재시작"
        }
    } else {
        Write-Host "[launcher] 종료 (exit $code) → 루프 탈출"
        break
    }
}
