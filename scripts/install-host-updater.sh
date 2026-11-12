#!/usr/bin/env bash

set -Eeuo pipefail

INSTALL_DIR="${INSTALL_DIR:-/opt/open-ai-canvas}"
REPOSITORY="${REPOSITORY:-benxh1024/self-open-ai-canvas}"
# Command-line environment takes priority. When omitted, use the deployment's
# persisted mode so a later updater reinstall cannot silently switch Compose files.
DEPLOYMENT_MODE="${CANVAS_DEPLOYMENT_MODE:-}"
UPDATER_RELEASE_TAG="${CANVAS_UPDATER_RELEASE_TAG:-}"
SOCKET_DIR="${CANVAS_UPDATER_SOCKET_DIR:-/run/open-ai-canvas-updater}"
UPDATER_BIN="/usr/local/bin/open-ai-canvas-host-updater"
UPDATER_ENV="/etc/open-ai-canvas-updater.env"
UPDATER_SERVICE="/etc/systemd/system/open-ai-canvas-updater.service"
GITHUB_TOKEN="${CANVAS_UPDATER_GITHUB_TOKEN:-}"

COMPOSE_FILE=""
MIGRATION_COMPOSE_FILE=""
DATABASE_MODE=""
IMAGE_REPOSITORY="${CANVAS_UPDATER_IMAGE_REPOSITORY:-}"

fail() {
    printf 'Host Updater 安装失败：%s\n' "$1" >&2
    exit 1
}

set_env_value() {
    local path="$1"
    local key="$2"
    local value="$3"
    local temporary

    [[ -f "$path" ]] || fail "环境文件不存在：$path"
    temporary="$(mktemp "${path}.XXXXXX")"
    awk -v key="$key" -v value="$value" '
        BEGIN { updated=0 }
        index($0, key "=") == 1 { print key "=" value; updated=1; next }
        { print }
        END { if (!updated) print key "=" value }
    ' "$path" > "$temporary"
    chmod --reference="$path" "$temporary"
    mv "$temporary" "$path"
}

require_root() {
    [[ "${EUID}" -eq 0 ]] || fail "请使用 sudo 运行"
    [[ "$(uname -s)" == "Linux" ]] || fail "仅支持 Linux 服务器"
    command -v systemctl >/dev/null 2>&1 || fail "服务器必须使用 systemd"
    command -v curl >/dev/null 2>&1 || fail "缺少 curl"
    command -v sha256sum >/dev/null 2>&1 || fail "缺少 sha256sum"
    command -v openssl >/dev/null 2>&1 || fail "缺少 openssl"
    command -v docker >/dev/null 2>&1 || fail "缺少 docker"
    [[ -f "${INSTALL_DIR}/.env" ]] || fail "未找到 ${INSTALL_DIR}/.env"
    if [[ -z "$DEPLOYMENT_MODE" ]]; then
        DEPLOYMENT_MODE="$(sed -n 's/^CANVAS_DEPLOYMENT_MODE=//p' "${INSTALL_DIR}/.env" | tail -n 1)"
    fi
    DEPLOYMENT_MODE="${DEPLOYMENT_MODE:-managed}"
    case "$DEPLOYMENT_MODE" in
        managed)
            COMPOSE_FILE="docker-compose.deploy.yml"
            MIGRATION_COMPOSE_FILE="$COMPOSE_FILE"
            DATABASE_MODE="managed"
            ;;
        external)
            COMPOSE_FILE="docker-compose.external-services.yml"
            MIGRATION_COMPOSE_FILE="docker-compose.external-migrate.yml"
            DATABASE_MODE="external"
            ;;
        *)
            fail "不支持的 CANVAS_DEPLOYMENT_MODE：${DEPLOYMENT_MODE}（可选 managed 或 external）"
            ;;
    esac
    [[ -f "${INSTALL_DIR}/${COMPOSE_FILE}" ]] || fail "未找到部署 Compose：${COMPOSE_FILE}"
    [[ -f "${INSTALL_DIR}/${MIGRATION_COMPOSE_FILE}" ]] || fail "未找到迁移 Compose：${MIGRATION_COMPOSE_FILE}"
    local backend_image web_image agent_image database_url redis_url
    backend_image="$(sed -n 's/^CANVAS_BACKEND_IMAGE=//p' "${INSTALL_DIR}/.env" | tail -n 1)"
    web_image="$(sed -n 's/^CANVAS_WEB_IMAGE=//p' "${INSTALL_DIR}/.env" | tail -n 1)"
    agent_image="$(sed -n 's/^CANVAS_YINGCE_AGENT_IMAGE=//p' "${INSTALL_DIR}/.env" | tail -n 1)"
    [[ "$backend_image" =~ ^ghcr\.io/[^[:space:]]+@sha256:[a-f0-9]{64}$ ]] || fail "CANVAS_BACKEND_IMAGE 必须固定为 GHCR digest"
    [[ "$web_image" =~ ^ghcr\.io/[^[:space:]]+@sha256:[a-f0-9]{64}$ ]] || fail "CANVAS_WEB_IMAGE 必须固定为 GHCR digest"
    [[ "$agent_image" =~ ^ghcr\.io/[^[:space:]]+@sha256:[a-f0-9]{64}$ ]] || fail "CANVAS_YINGCE_AGENT_IMAGE 必须固定为 GHCR digest"
    if [[ "$DEPLOYMENT_MODE" == "external" ]]; then
        database_url="$(sed -n 's/^DATABASE_URL=//p' "${INSTALL_DIR}/.env" | tail -n 1)"
        redis_url="$(sed -n 's/^REDIS_URL=//p' "${INSTALL_DIR}/.env" | tail -n 1)"
        [[ -n "$database_url" ]] || fail "external 模式必须配置 DATABASE_URL"
        [[ -n "$redis_url" ]] || fail "external 模式必须配置 REDIS_URL"
        docker network inspect db-common-network >/dev/null 2>&1 || fail "external 模式要求 Docker 网络 db-common-network；请先执行：docker network create db-common-network"
    fi
    if [[ -z "$IMAGE_REPOSITORY" ]]; then
        IMAGE_REPOSITORY="ghcr.io/${REPOSITORY%%/*}/self-open-ai-canvas"
    fi
}

read_image_tag() {
	local configured updater_tag
    configured="$(sed -n 's/^CANVAS_IMAGE_TAG=//p' "${INSTALL_DIR}/.env" | tail -n 1)"
    [[ -n "$configured" && "$configured" != "latest" ]] || fail "请先把 CANVAS_IMAGE_TAG 固定为已发布版本"
	updater_tag="${UPDATER_RELEASE_TAG:-$configured}"
	[[ "$updater_tag" =~ ^v?[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]] || fail "CANVAS_UPDATER_RELEASE_TAG 不是有效的 Release 标签"
	if [[ "$updater_tag" == v* ]]; then
		RELEASE_TAG="$updater_tag"
    else
		RELEASE_TAG="v${updater_tag}"
    fi
}

load_github_token() {
    if [[ -z "$GITHUB_TOKEN" && -f "$UPDATER_ENV" ]]; then
        GITHUB_TOKEN="$(sed -n 's/^CANVAS_UPDATER_GITHUB_TOKEN=//p' "$UPDATER_ENV" | tail -n 1)"
    fi
}

download_github_file() {
    local url="$1" output="$2" accept_header="${3:-}" redirect_url response_meta status result
    local -a curl_args=(--show-error --silent --proto '=https')
    local -a extra_headers=(--header 'User-Agent: open-ai-canvas-host-updater')
    if [[ -n "$accept_header" ]]; then
        extra_headers+=(--header "$accept_header")
    fi
    if [[ -n "$GITHUB_TOKEN" ]]; then
        # Authenticate only the initial GitHub request. The subsequent signed
        # asset URL must not receive the PAT through a cross-host redirect.
        if response_meta="$(printf 'Authorization: Bearer %s\n' "$GITHUB_TOKEN" \
            | curl "${curl_args[@]}" --header @- "${extra_headers[@]}" \
                --write-out '%{redirect_url}\t%{http_code}' --output /dev/null "$url")"; then
            redirect_url="${response_meta%$'\t'*}"
            status="${response_meta##*$'\t'}"
            if [[ "$status" =~ ^3[0-9]{2}$ && -n "$redirect_url" ]]; then
                if curl "${curl_args[@]}" --fail --location --max-redirs 10 "$redirect_url" -o "$output"; then
                    return 0
                else
                    result=$?
                fi
            elif [[ "$status" =~ ^2[0-9]{2}$ ]]; then
                # Some GitHub/proxy responses return the asset directly.
                if status="$(printf 'Authorization: Bearer %s\n' "$GITHUB_TOKEN" \
                    | curl "${curl_args[@]}" --header @- "${extra_headers[@]}" \
                        --write-out '%{http_code}' "$url" -o "$output")" && [[ "$status" =~ ^2[0-9]{2}$ ]]; then
                    return 0
                else
                    result=22
                fi
            else
                result=22
            fi
        else
            result=$?
        fi
    else
        if status="$(curl "${curl_args[@]}" --fail --location --max-redirs 10 --write-out '%{http_code}' "$url" -o "$output")"; then
            return 0
        else
            result=$?
        fi
    fi
    printf 'GitHub 下载失败（HTTP %s，curl exit %s）：%s\n' "${status:-无响应}" "$result" "$url" >&2
    return "$result"
}

release_asset_id() {
    local release_json="$1" asset_name="$2"
    if command -v jq >/dev/null 2>&1; then
        jq -r --arg name "$asset_name" '.assets[]? | select(.name == $name) | .id' "$release_json" | head -n 1
        return 0
    fi
    if command -v python3 >/dev/null 2>&1; then
        python3 - "$release_json" "$asset_name" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as stream:
    release = json.load(stream)
for asset in release.get("assets", []):
    if asset.get("name") == sys.argv[2]:
        print(asset.get("id", ""))
        break
PY
        return 0
    fi
    fail "私有 GitHub Release 下载需要 jq 或 python3 才能解析资产列表"
}

download_release_asset() {
    local asset_name="$1" output="$2" release_json asset_id
    if [[ -z "$GITHUB_TOKEN" ]]; then
        download_github_file \
            "https://github.com/${REPOSITORY}/releases/download/${RELEASE_TAG}/${asset_name}" \
            "$output"
        return
    fi

    release_json="$(mktemp)"
    if ! download_github_file \
        "https://api.github.com/repos/${REPOSITORY}/releases/tags/${RELEASE_TAG}" \
        "$release_json" \
        "Accept: application/vnd.github+json"; then
        rm -f "$release_json"
        return 1
    fi
    asset_id="$(release_asset_id "$release_json" "$asset_name")"
    rm -f "$release_json"
    if [[ ! "$asset_id" =~ ^[0-9]+$ ]]; then
        printf 'GitHub Release %s 中不存在资产 %s\n' "$RELEASE_TAG" "$asset_name" >&2
        return 1
    fi
    download_github_file \
        "https://api.github.com/repos/${REPOSITORY}/releases/assets/${asset_id}" \
        "$output" \
        "Accept: application/octet-stream"
}

install_binary() {
    local arch asset temporary checksum_file expected
    case "$(uname -m)" in
        x86_64|amd64) arch="amd64" ;;
        aarch64|arm64) arch="arm64" ;;
        *) fail "不支持的 CPU 架构：$(uname -m)" ;;
    esac
    asset="open-ai-canvas-host-updater-linux-${arch}"
    temporary="$(mktemp)"
    checksum_file="$(mktemp)"
    download_release_asset "$asset" "$temporary" \
        || fail "下载 Host Updater 失败；私有仓库请设置 CANVAS_UPDATER_GITHUB_TOKEN"
    download_release_asset "SHA256SUMS" "$checksum_file" \
        || fail "下载 Release 校验清单失败；私有仓库请设置 CANVAS_UPDATER_GITHUB_TOKEN"
    expected="$(awk -v asset="$asset" '$2 == asset { print $1 }' "$checksum_file")"
    [[ "$expected" =~ ^[a-f0-9]{64}$ ]] || fail "Release 校验清单缺少 ${asset}"
    printf '%s  %s\n' "$expected" "$temporary" | sha256sum -c - >/dev/null || fail "Host Updater SHA-256 校验失败"
    install -m 0755 "$temporary" "$UPDATER_BIN"
    rm -f "$temporary" "$checksum_file"
}

ensure_token() {
    local token temporary
    token="$(sed -n 's/^CANVAS_UPDATER_TOKEN=//p' "${INSTALL_DIR}/.env" | tail -n 1)"
    if [[ -z "$token" ]]; then
        token="$(openssl rand -hex 32)"
        temporary="$(mktemp "${INSTALL_DIR}/.env.XXXXXX")"
        awk -v token="$token" '
            BEGIN { updated=0 }
            /^CANVAS_UPDATER_TOKEN=/ { print "CANVAS_UPDATER_TOKEN=" token; updated=1; next }
            { print }
            END { if (!updated) print "CANVAS_UPDATER_TOKEN=" token }
        ' "${INSTALL_DIR}/.env" > "$temporary"
        chmod --reference="${INSTALL_DIR}/.env" "$temporary"
        mv "$temporary" "${INSTALL_DIR}/.env"
    fi
    [[ ${#token} -ge 32 ]] || fail "CANVAS_UPDATER_TOKEN 长度不足"
    set_env_value "${INSTALL_DIR}/.env" "CANVAS_DEPLOYMENT_MODE" "$DEPLOYMENT_MODE"
    umask 077
    printf '%s\n' \
        "CANVAS_UPDATER_TOKEN=$token" \
        "CANVAS_UPDATER_GITHUB_TOKEN=$GITHUB_TOKEN" \
        "CANVAS_UPDATER_INSTALL_DIR=$INSTALL_DIR" \
        "CANVAS_UPDATER_SOCKET=$SOCKET_DIR/updater.sock" \
        "CANVAS_UPDATER_REPOSITORY=$REPOSITORY" \
        "CANVAS_UPDATER_IMAGE_REPOSITORY=$IMAGE_REPOSITORY" \
        "CANVAS_UPDATER_COMPOSE_FILE=$COMPOSE_FILE" \
        "CANVAS_UPDATER_MIGRATION_COMPOSE_FILE=$MIGRATION_COMPOSE_FILE" \
        "CANVAS_UPDATER_DATABASE_MODE=$DATABASE_MODE" > "$UPDATER_ENV"
    chmod 600 "$UPDATER_ENV"
}

install_service() {
    local temporary_service protect_home=true
    case "$INSTALL_DIR" in
        /root|/root/*|/home|/home/*|/run/user|/run/user/*)
            # ProtectHome=true hides these paths entirely. Keep the rest of
            # the home tree read-only while ReadWritePaths grants the updater
            # access to the selected deployment directory.
            protect_home=read-only
            ;;
    esac
    install -d -m 0755 "$SOCKET_DIR"
    install -d -m 0700 /var/lib/open-ai-canvas-updater "${INSTALL_DIR}/backups"
    temporary_service="$(mktemp)"
    printf '%s\n' \
        '[Unit]' \
        'Description=Open AI Canvas Host Updater' \
        'After=docker.service network-online.target' \
        'Requires=docker.service' \
        'Wants=network-online.target' \
        '' \
        '[Service]' \
        'Type=simple' \
        "EnvironmentFile=${UPDATER_ENV}" \
        "ExecStart=${UPDATER_BIN}" \
        'Restart=on-failure' \
        'RestartSec=5s' \
        'NoNewPrivileges=true' \
        'PrivateTmp=true' \
        "ProtectHome=${protect_home}" \
        'ProtectSystem=full' \
        "ReadWritePaths=${INSTALL_DIR} /var/lib/open-ai-canvas-updater ${SOCKET_DIR} /usr/local/bin" \
        '' \
        '[Install]' \
        'WantedBy=multi-user.target' > "$temporary_service"
    install -m 0644 "$temporary_service" "$UPDATER_SERVICE"
    rm -f "$temporary_service"
    systemctl daemon-reload
    systemctl enable --now open-ai-canvas-updater.service
    systemctl restart open-ai-canvas-updater.service
}

recreate_backend() {
    CANVAS_UPDATER_SOCKET_DIR="$SOCKET_DIR" docker compose --env-file "${INSTALL_DIR}/.env" \
        -f "${INSTALL_DIR}/${COMPOSE_FILE}" \
        up -d --no-deps --force-recreate backend || fail "重建 backend 容器失败，请检查 Docker Compose 日志"
}

main() {
    require_root
    load_github_token
    read_image_tag
    install_binary
    ensure_token
    install_service
    recreate_backend
    printf 'Host Updater 已安装，部署模式：%s，Socket：%s/updater.sock\n' "$DEPLOYMENT_MODE" "$SOCKET_DIR"
    if [[ "$DEPLOYMENT_MODE" == "external" ]]; then
        printf '外部 PostgreSQL/Redis 不会由更新器创建；备份和恢复通过 %s 的 db-tools 容器执行。\n' "$MIGRATION_COMPOSE_FILE"
    fi
}

main "$@"
