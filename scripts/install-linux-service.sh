#!/usr/bin/env bash
# aglink systemd user 서비스 설치 스크립트 (Linux)
# 대상 머신에서 직접 실행하세요.
# 사용법: bash install-linux-service.sh [BINARY_PATH]
#   BINARY_PATH: aglink 바이너리 경로 (기본: ~/aglink)
#
# 특징:
#   - systemd user 서비스 (루트 권한 불필요)
#   - 로그: ~/.aglink/logs/
#   - 자동 재시작 on-failure
#   - 부팅 시 자동 시작 (loginctl enable-linger)

set -euo pipefail

# NOTE: the binary must NOT live at ~/aglink — that path is the service's default
# working home (see defaultHomeDir in homedir.go), so a file there makes every
# worker's cwd a non-directory and claude fails to exec with ENOTDIR.
BINARY="${1:-$HOME/bin/aglink}"
SERVICE_NAME="aglink"
SERVICE_FILE="${HOME}/.config/systemd/user/${SERVICE_NAME}.service"

# Data dir must match dataDir() in config.go: ~/.aglink normally, but a
# pre-rename ~/.teleclaude is used in place when ~/.aglink does not exist.
# Creating ~/.aglink here on a legacy install would silently strand the old
# store.json / tasks.json / history the service is still meant to read.
DATA_DIR="${HOME}/.aglink"
if [[ ! -d "${DATA_DIR}" && -d "${HOME}/.teleclaude" ]]; then
    DATA_DIR="${HOME}/.teleclaude"
    echo "ℹ  pre-rename 데이터 디렉터리 사용: ${DATA_DIR}"
fi
LOG_DIR="${DATA_DIR}/logs"
CONFIG_FILE="${DATA_DIR}/config.txt"

# 사전 검사
if [[ ! -x "${BINARY}" ]]; then
    echo "❌ 바이너리를 찾을 수 없거나 실행 권한이 없습니다: ${BINARY}"
    echo "   deploy-linux.sh 로 먼저 배포하거나 chmod +x ${BINARY} 실행"
    exit 1
fi

if [[ ! -f "${CONFIG_FILE}" && ! -f "${DATA_DIR}/config.yaml" ]]; then
    echo "⚠  설정 파일이 없습니다: ${CONFIG_FILE}"
    echo "   서비스 등록 전에 다음 중 하나를 실행하세요:"
    echo "     1) ${BINARY} run      (설정 마법사 — 대화형 터미널 필요)"
    echo "     2) cp config.example.txt ${CONFIG_FILE}  (수동 편집)"
    read -rp "   계속 진행할까요? [y/N]: " yn
    [[ "${yn,,}" == "y" ]] || exit 1
fi

# PATH에 claude CLI가 있는지 확인
CLAUDE_PATH="$(which claude 2>/dev/null || true)"
if [[ -z "${CLAUDE_PATH}" ]]; then
    echo "⚠  claude CLI를 PATH에서 찾을 수 없습니다."
    echo "   NVM을 사용한다면 PATH에 node bin 경로를 추가하거나 config.txt에 CLAUDE_PATH= 설정하세요."
fi

mkdir -p "${LOG_DIR}"
mkdir -p "$(dirname "${SERVICE_FILE}")"

# 기존 서비스 중단
if systemctl --user is-active "${SERVICE_NAME}.service" &>/dev/null; then
    echo "▶ 기존 서비스 중단 중..."
    systemctl --user stop "${SERVICE_NAME}.service"
fi

# PATH 구성: NVM 경로 포함
NVM_BIN=""
if [[ -d "${HOME}/.nvm/versions/node" ]]; then
    # 최신 node 버전의 bin 경로 자동 포함
    LATEST_NODE="$(ls -v "${HOME}/.nvm/versions/node" | tail -1)"
    if [[ -n "${LATEST_NODE}" ]]; then
        NVM_BIN=":${HOME}/.nvm/versions/node/${LATEST_NODE}/bin"
    fi
fi
SERVICE_PATH="/usr/local/bin:/usr/bin:/bin:${HOME}/.local/bin${NVM_BIN}"

cat > "${SERVICE_FILE}" << EOF
[Unit]
Description=Aglink - Telegram Claude Agent
After=network.target

[Service]
Type=simple
ExecStart=${BINARY} run
WorkingDirectory=${HOME}
Restart=on-failure
RestartSec=5
KillMode=process
Environment=HOME=${HOME}
Environment=PATH=${SERVICE_PATH}
StandardOutput=append:${LOG_DIR}/aglink.log
StandardError=append:${LOG_DIR}/aglink.error.log

[Install]
WantedBy=default.target
EOF

echo "▶ 서비스 파일 생성: ${SERVICE_FILE}"

# linger 활성화 (로그아웃 후에도 서비스 유지)
if loginctl enable-linger "${USER}" 2>/dev/null; then
    echo "▶ loginctl enable-linger: 로그아웃 후에도 서비스 유지 활성화"
fi

systemctl --user daemon-reload
systemctl --user enable "${SERVICE_NAME}.service"
systemctl --user start "${SERVICE_NAME}.service"

sleep 2
echo ""
systemctl --user status "${SERVICE_NAME}.service" --no-pager

echo ""
echo "✅ aglink 서비스 설치 완료"
echo "   로그:    tail -f ${LOG_DIR}/aglink.error.log"
echo "   중단:    systemctl --user stop aglink"
echo "   재시작:  systemctl --user restart aglink"
echo "   제거:    systemctl --user disable --now aglink && rm ${SERVICE_FILE}"
