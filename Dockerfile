# Copyright (c) 2026 Hygon Information Technology Co., Ltd.
# SPDX-License-Identifier: Apache-2.0

FROM ubuntu:22.04

COPY LICENSE NOTICE THIRD_PARTY_NOTICES.md /licenses/

WORKDIR /usr/local/bin

RUN apt-get update && apt-get install -y --no-install-recommends \
  ca-certificates \
  kmod \
  pciutils \
  libdrm2 \
  && rm -rf /var/lib/apt/lists/*

# Host-side build output (see scripts/build.sh binary).
COPY bin/hcu-dra-driver .

RUN chmod +x /usr/local/bin/hcu-dra-driver

ENV LD_LIBRARY_PATH=${LD_LIBRARY_PATH}:/opt/hyhal/lib:/usr/local/lib

ENTRYPOINT ["/usr/local/bin/hcu-dra-driver"]
