#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

docker rm -f mfcc-nginx 2>/dev/null || true
docker run -d -p 58881:58881 --name mfcc-nginx --network host mfcc-nginx
