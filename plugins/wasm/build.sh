#!/bin/sh
# Builds every plugin in plugins/wasm into plugins/wasm/dist/<name>.wasm.
#   plugins/wasm/build.sh            # all plugins
#   plugins/wasm/build.sh pii-redactor
# Point RELAYOPS_WASM_PLUGINS_DIR at the dist directory (or copy the files
# into the gateway image) and restart the gateway.
set -eu
cd "$(dirname "$0")/../.."
out=plugins/wasm/dist
mkdir -p "$out"
names=${*:-$(cd plugins/wasm && ls -d */ | tr -d / | grep -v -e '^dist$' -e '^internal$')}
for n in $names; do
  GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -trimpath -ldflags=-s -o "$out/$n.wasm" "./plugins/wasm/$n"
  echo "built $out/$n.wasm"
done
