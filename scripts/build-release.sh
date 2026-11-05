#!/usr/bin/env bash
# 交叉编译并打包发布产物。
#
# 每个平台一个压缩包，内含二进制 + 示例配置 + 说明。
# 只发裸二进制会让下载的人不知道要建配置文件、放在哪——这正是 v0.1.0 的问题。
#
# 用法：
#   scripts/build-release.sh 0.2.0          # 输出到 dist/
#   VERSION=0.2.0 scripts/build-release.sh  # 同上
set -euo pipefail

cd "$(dirname "$0")/.."

VER="${1:-${VERSION:-}}"
if [ -z "$VER" ]; then
  echo "用法: scripts/build-release.sh <版本号，如 0.2.0>" >&2
  exit 1
fi
TAG="v${VER#v}"
OUT="dist"

rm -rf "$OUT"
mkdir -p "$OUT"
# 暂存目录：包内根目录叫 gsh-<tag>-<平台>，与 dist 根下的二进制不冲突。
STAGE="$OUT/.stage"
rm -rf "$STAGE"
mkdir -p "$STAGE"

# 构建期校验器：产物必须静态链接。同平台编译时 CGO 默认为 1，
# 一旦漏掉 CGO_ENABLED=0，产物会绑定 runner 的 glibc，别人的旧机器直接
# "GLIBC_2.34 not found"。这个错误只在用户机器上暴露，所以必须在这里拦。
CHK="$OUT/elfcheck"
go build -trimpath -o "$CHK" ./scripts/elfcheck
# set -e 下不能写 `[ -f x ] && y`：条件为假时整条返回非零会直接退出脚本。
if [ -f "${CHK}.exe" ]; then
  CHK="${CHK}.exe"
fi

# 目标平台：GOOS GOARCH [GOARM]
targets=(
  "windows amd64"
  "windows 386"
  "windows arm64"
  "linux amd64"
  "linux 386"
  "linux arm64"
  "linux arm 7"
  "linux loong64"
)

stage() {
  # stage <目录> <二进制>：放入运行时需要的全部文件
  local dir="$1" bin="$2"
  mkdir -p "$dir"
  cp "$bin" "$dir/"
  cp config.example.json "$dir/"
  cp README.md "$dir/"
}

package() {
  # package <平台标签> <包内目录名>
  local label="$1" name="$2"
  if [[ "$label" == windows-* ]]; then
    if command -v zip >/dev/null 2>&1; then
      (cd "$STAGE" && zip -qr "../${label}.zip" "$name")
    else
      # Windows 本机通常没有 zip：用系统自带的 Compress-Archive。
      local wdir wout
      wdir="$(cygpath -w "$STAGE/$name")"
      wout="$(cygpath -w "$OUT/${label}.zip")"
      powershell -NoProfile -Command "Compress-Archive -Path '$wdir' -DestinationPath '$wout' -Force"
    fi
  else
    tar -czf "$OUT/${label}.tar.gz" -C "$STAGE" "$name"
  fi
}

for t in "${targets[@]}"; do
  read -r goos goarch goarm <<<"$t"
  ext=""
  [ "$goos" = "windows" ] && ext=".exe"
  label="${goos}-${goarch}${goarm:+v${goarm}}"
  bin="${OUT}/gsh-${TAG}-${label}${ext}"

  echo ">> 编译 ${goos}/${goarch}${goarm:+ goarm=$goarm}"
  # CGO_ENABLED=0：不链接 libc，产物静态、不绑定 glibc 版本。
  # GOAMD64 / GOARM64 显式钉在最低档：这两个变量会被宿主环境继承，
  # 一旦是从别处继承来的 v3，老 CPU 上会直接 illegal instruction。
  build=(env GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0
    GOAMD64=v1 GOARM64=v8.0
    go build -trimpath -ldflags "-s -w -X main.version=${VER}" -o "$bin" .)
  if [ -n "${goarm:-}" ]; then
    build=(env GOOS="$goos" GOARCH="$goarch" GOARM="$goarm" CGO_ENABLED=0
      GOAMD64=v1 GOARM64=v8.0
      go build -trimpath -ldflags "-s -w -X main.version=${VER}" -o "$bin" .)
  fi
  "${build[@]}"

  # 只对 Linux 产物验静态链接。Windows 的 PE 不适用这套判断。
  if [ "$goos" = "linux" ]; then
    "$CHK" "$bin"
  fi

  pkgname="gsh-${TAG}-${label}"
  stage "$STAGE/$pkgname" "$bin"
  package "$label" "$pkgname"
  rm -rf "$STAGE/$pkgname" "$bin"
done

rm -f "$CHK" "${CHK%.exe}.exe"
rmdir "$STAGE" 2>/dev/null || true

echo
echo "产物："
ls -lh "$OUT" | tail -n +2
