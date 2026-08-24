#!/usr/bin/env bash
# aglink Linux 배포 스크립트
# 사용법: ./scripts/deploy-linux.sh [SSH_HOST] [REMOTE_PATH]
#   SSH_HOST   : 배포 대상 SSH alias 또는 user@host (기본: nanopi)
#   REMOTE_PATH: 바이너리 설치 경로 (기본: ~/aglink)
#
# 사전 준비:
#   - GOOS/GOARCH를 대상 아키텍처에 맞게 설정 (ARM64: arm64, x86-64: amd64)
#   - SSH key 인증 설정 완료
#
# 예시:
#   ./scripts/deploy-linux.sh                          # nanopi (ARM64)
#   ./scripts/deploy-linux.sh user@192.168.1.100       # IP 직접 지정
#   GOARCH=amd64 ./scripts/deploy-linux.sh myserver    # x86-64 서버

set -euo pipefail

SSH_HOST="${1:-nanopi}"
# NOT ~/aglink: that path is the service's default working home (defaultHomeDir
# in homedir.go). Dropping the binary there makes every worker's cwd a file, and
# claude then fails to start with "fork/exec ...: not a directory".
REMOTE_PATH="${2:-~/bin/aglink}"
GOARCH="${GOARCH:-arm64}"
BINARY="aglink-linux-${GOARCH}"

echo "▶ 빌드: linux/${GOARCH} → ${BINARY}"
GOOS=linux GOARCH="${GOARCH}" go build -o "${BINARY}" ./...

echo "▶ 배포: ${BINARY} → ${SSH_HOST}:${REMOTE_PATH}"
ssh "${SSH_HOST}" "mkdir -p \$(dirname ${REMOTE_PATH})"
scp "${BINARY}" "${SSH_HOST}:${REMOTE_PATH}"
ssh "${SSH_HOST}" "chmod +x ${REMOTE_PATH}"

# 서비스가 등록되어 있으면 재시작
if ssh "${SSH_HOST}" "systemctl --user is-enabled aglink.service &>/dev/null"; then
    echo "▶ 서비스 재시작: aglink.service"
    ssh "${SSH_HOST}" "systemctl --user restart aglink.service && systemctl --user status aglink.service --no-pager -l"
else
    echo "ℹ  서비스 미등록 — 수동으로 실행하거나 install-linux-service.sh 를 실행하세요"
    echo "   ssh ${SSH_HOST} '${REMOTE_PATH} run'"
fi

echo "✅ 배포 완료"
