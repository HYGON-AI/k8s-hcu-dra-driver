#!/usr/bin/env bash
# Copyright (c) 2026 Hygon Information Technology Co., Ltd.
# SPDX-License-Identifier: Apache-2.0

# Host-side build pipeline for hcu-dra-driver.
# Usage:
#   ./build.sh [all|binary|image|tar]
#     all    - compile, docker build, save tar (default)
#     binary - compile only → bin/hcu-dra-driver
#     image  - docker build (requires bin/hcu-dra-driver)
#     tar    - docker save image tar → dist/ (requires local image)
#
# Environment:
#   IMAGE_REGISTRY, IMAGE_REPO, IMAGE_TAG, GOPROXY, CGO_ENABLED
#
# On CRLF-related "invalid option name: pipefail", run:
#   sed -i 's/\r$//' build.sh scripts/build.sh
set -eu

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODULE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

export GOPROXY="${GOPROXY:-https://goproxy.cn}"
export CGO_ENABLED="${CGO_ENABLED:-1}"

BIN_DIR="${MODULE_DIR}/bin"
DIST_DIR="${MODULE_DIR}/dist"
BIN_NAME="hcu-dra-driver"
BIN_PATH="${BIN_DIR}/${BIN_NAME}"
MAIN_PKG="./cmd/hcu-dra-driver"

IMAGE_REGISTRY="${IMAGE_REGISTRY:-harbor.sourcefind.cn:5443}"
IMAGE_REPO="${IMAGE_REPO:-hcu/admin/base/hcu-dra-driver}"
IMAGE_TAG="${IMAGE_TAG:-v1.0.0}"
IMAGE="${IMAGE_REGISTRY}/${IMAGE_REPO}:${IMAGE_TAG}"
TAR_NAME="hcu-dra-driver-${IMAGE_TAG}.tar"
TAR_PATH="${DIST_DIR}/${TAR_NAME}"

usage() {
  cat <<EOF
Usage: $(basename "$0") [all|binary|image|tar]

  all     compile binary, build image, export tar (default)
  binary  compile ${BIN_NAME} to bin/
  image   build docker image (needs bin/${BIN_NAME})
  tar     save image to dist/${TAR_NAME}

Environment: IMAGE_TAG, IMAGE_REGISTRY, IMAGE_REPO, GOPROXY, CGO_ENABLED
EOF
}

build_binary() {
  echo "==> Build binary: ${BIN_PATH}"
  mkdir -p "${BIN_DIR}"
  cd "${MODULE_DIR}"
  go mod tidy
  # Requires Linux host with CGO and hcu-dcgm headers/libs.
  go build -ldflags="-s -w" -o "${BIN_PATH}" "${MAIN_PKG}"
  echo "    Binary: ${BIN_PATH}"
}

build_image() {
  if [[ ! -f "${BIN_PATH}" ]]; then
    echo "error: missing ${BIN_PATH}; run '$(basename "$0") binary' first" >&2
    exit 1
  fi
  echo "==> Build docker image: ${IMAGE}"
  docker build -t "${IMAGE}" -f "${MODULE_DIR}/Dockerfile" "${MODULE_DIR}"
}

save_tar() {
  echo "==> Save image tar: ${TAR_PATH}"
  mkdir -p "${DIST_DIR}"
  docker save -o "${TAR_PATH}" "${IMAGE}"
  echo "    Tar: ${TAR_PATH}"
}

main() {
  local target="${1:-all}"
  case "${target}" in
    all)
      build_binary
      build_image
      save_tar
      echo "Done: ${IMAGE}"
      ;;
    binary)
      build_binary
      ;;
    image)
      build_image
      ;;
    tar)
      save_tar
      ;;
    -h|--help|help)
      usage
      ;;
    *)
      echo "error: unknown target '${target}'" >&2
      usage >&2
      exit 1
      ;;
  esac
}

main "$@"
