#!/usr/bin/env bash
# build.sh — compile the maild daemon and bundle release artifacts.
#
# Steps:
#   1. Re-generate the gRPC stub from proto/v1/mail.proto so binary and
#      proto stay in sync.
#   2. go build the daemon binary into the stage directory, injecting
#      Version/Commit/Date via -ldflags -X.
#   3. Stage runtime assets (toml example, systemd unit) alongside the binary
#      and tar the result. cert.sh is not shipped: it runs on the signing
#      machine, not on the gateway.
#
# Usage:
#   ./build.sh                            # linux/amd64, compile only (no tarball)
#   ./build.sh --arm64                    # cross-compile linux/arm64
#   ./build.sh --dev                      # stamp full UTC datetime instead of YYMMDD
#   ./build.sh --tar                      # additionally pack stage into maild.tar.gz
#   ./build.sh --build-dir=/path/to/out   # override build output dir (default: <project>/dist)
#                                         # relative paths resolve against the current working dir
#
# Requires: protoc, protoc-gen-go, protoc-gen-go-grpc, go.

set -euo pipefail

GOARCH_TARGET="amd64"
BUILD_DIR=""
DEV_DATE=0
DO_TAR=0
for arg in "$@"; do
    case "$arg" in
        --arm64)
            GOARCH_TARGET="arm64"
            ;;
        --amd64)
            GOARCH_TARGET="amd64"
            ;;
        --dev)
            DEV_DATE=1
            ;;
        --tar)
            DO_TAR=1
            ;;
        --build-dir=*)
            BUILD_DIR="${arg#--build-dir=}"
            if [[ -z "${BUILD_DIR}" ]]; then
                echo "--build-dir requires a non-empty path" >&2
                exit 1
            fi
            ;;
        -h|--help)
            sed -n '2,20p' "$0"
            exit 0
            ;;
        *)
            echo "unknown argument: $arg" >&2
            echo "usage: $0 [--arm64|--amd64] [--dev] [--tar] [--build-dir=<path>]" >&2
            exit 1
            ;;
    esac
done

# 記下呼叫 build.sh 當下的工作目錄，供相對的 --build-dir 解析使用。
INVOCATION_DIR="$(pwd)"
cd "$(dirname "$0")"

VERSION="$(git describe --tags --abbrev=0 2>/dev/null || echo dev)"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
case "${VERSION}" in
    dev*) DEV_DATE=1 ;;
esac
if [ "${DEV_DATE}" = "1" ]; then
    DATE="$(date -u +%y%m%dT%H%M%SZ)"
else
    DATE="$(date -u +%y%m%d)"
fi

VERSION_PKG="maild/pkg/version"
LDFLAGS="-s -w \
    -X ${VERSION_PKG}.Version=${VERSION} \
    -X ${VERSION_PKG}.Commit=${COMMIT} \
    -X ${VERSION_PKG}.Date=${DATE}"

PROJECT_ROOT="$(pwd)"
if [[ -z "${BUILD_DIR}" ]]; then
    BUILD_DIR="${PROJECT_ROOT}/dist"
elif [[ "${BUILD_DIR}" != /* ]]; then
    BUILD_DIR="${INVOCATION_DIR}/${BUILD_DIR}"
fi
PROTO_DIR="${PROJECT_ROOT}/proto/v1"
STUB_DIR="${PROJECT_ROOT}/pkg/grpc/mail"
STAGE_DIR="${BUILD_DIR}/maild"

TOTAL_STEPS=3
if [ "${DO_TAR}" = "1" ]; then
    TOTAL_STEPS=4
fi

echo "[1/${TOTAL_STEPS}] regenerating proto stub from ${PROTO_DIR}/mail.proto"
mkdir -p "${STUB_DIR}"
protoc \
    --go_out="${STUB_DIR}"      --go_opt=paths=source_relative \
    --go-grpc_out="${STUB_DIR}" --go-grpc_opt=paths=source_relative \
    -I"${PROTO_DIR}" \
    "${PROTO_DIR}/mail.proto"

echo "[2/${TOTAL_STEPS}] staging release directory at ${STAGE_DIR}"
mkdir -p "${BUILD_DIR}"
rm -rf "${STAGE_DIR}"
# data/ 必須存在：systemd unit 的 ReadWritePaths 指向它，不存在時服務無法啟動（226/NAMESPACE）。
mkdir -p "${STAGE_DIR}/conf" "${STAGE_DIR}/certs" "${STAGE_DIR}/data" "${STAGE_DIR}/systemd"

echo "[3/${TOTAL_STEPS}] building maild binary into stage (linux/${GOARCH_TARGET}) version=${VERSION} commit=${COMMIT} date=${DATE}"
CGO_ENABLED=0 GOOS=linux GOARCH="${GOARCH_TARGET}" \
    go build -trimpath -ldflags="${LDFLAGS}" -o "${STAGE_DIR}/maild" .
cp "${PROJECT_ROOT}/conf/maild.toml" "${STAGE_DIR}/conf/maild.toml.example"
cp "${PROJECT_ROOT}/systemd/maild.service" "${STAGE_DIR}/systemd/maild.service"

ARCHIVE=""
if [ "${DO_TAR}" = "1" ]; then
    ARCHIVE="${BUILD_DIR}/maild.tar.gz"
    echo "[4/${TOTAL_STEPS}] packing ${ARCHIVE}"
    tar -czf "${ARCHIVE}" -C "${BUILD_DIR}" maild
fi

echo "done."
echo "  staged:   ${STAGE_DIR}"
if [ -n "${ARCHIVE}" ]; then
    echo "  archive:  ${ARCHIVE}"
fi
