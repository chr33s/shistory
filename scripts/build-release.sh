#!/usr/bin/env bash
set -euo pipefail
mkdir -p dist
targets=(darwin/arm64 darwin/amd64 linux/amd64 linux/arm64)
for target in "${targets[@]}"; do
  platform=${target%/*}
  arch=${target#*/}
  name=shistory-$platform-$arch
  mkdir -p "dist/$name/shell/completions"
  CGO_ENABLED=0 GOOS=$platform GOARCH=$arch go build -trimpath -o "dist/$name/shistory" ./cmd/shistory
  cp shell/shistory.bash shell/shistory.zsh "dist/$name/shell/"
  cp shell/completions/_shistory "dist/$name/shell/completions/"
  cp shistory.plugin.zsh readme.md LICENSE "dist/$name/"
done
if [[ "${MACOS_SIGNING_REQUIRED:-0}" == 1 ]]; then
  bash scripts/sign-macos-release.sh
fi
archives=()
for target in "${targets[@]}"; do
  name=shistory-${target%/*}-${target#*/}
  tar -czf "dist/$name.tar.gz" -C dist "$name"
  archives+=("$name.tar.gz")
done
(
  cd dist
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${archives[@]}" > SHA256SUMS
  else
    shasum -a 256 "${archives[@]}" > SHA256SUMS
  fi
)
