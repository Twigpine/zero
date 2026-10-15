#!/usr/bin/env bash
# publish-packages.sh: publish the npm payloads that build-platform-packages.mjs
# assembled under <npm-dir> (wrapper/ plus platforms/*/), in the order and
# with the dist-tags docs/NPM_PACKAGING.md requires:
#
#   1. every platform payload, under the `platform` dist-tag, never `latest`;
#   2. check that `latest` was not moved onto a platform version;
#   3. the wrapper, which takes `latest`, then confirm `latest` is its version.
#
# The package name comes from the assembled wrapper's package.json, so the
# same script publishes whatever name the payloads were built under.
#
# A version that is already on the registry is skipped, which makes
# re-running a release job that failed halfway safe: the platform payloads
# that went out are not published twice, and the wrapper still follows them.
# Publishes pass --ignore-scripts because the payloads are already assembled
# and verified; nothing should run at pack time.
#
# Usage: scripts/npm/publish-packages.sh <npm-dir>
#
# Auth comes from the environment (npm OIDC trusted publishing in CI).
# ZERO_NPM_PUBLISH_RETRY_DELAY overrides the seconds between dist-tag lookups.
set -euo pipefail

fail() {
  echo "::error::$*" >&2
  exit 1
}

npm_dir="${1:-}"
[ -n "$npm_dir" ] || fail "usage: publish-packages.sh <npm-dir>"
wrapper_dir="${npm_dir%/}/wrapper"
[ -f "${wrapper_dir}/package.json" ] || fail "${wrapper_dir}/package.json not found; run build-platform-packages.mjs first"

manifest_field() {
  node -e 'const value = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"))[process.argv[2]]; process.stdout.write(value === undefined ? "" : String(value));' "$1/package.json" "$2"
}

name="$(manifest_field "$wrapper_dir" name)"
version="$(manifest_field "$wrapper_dir" version)"
[ -n "$name" ] && [ -n "$version" ] || fail "${wrapper_dir}/package.json has no name or version"
delay="${ZERO_NPM_PUBLISH_RETRY_DELAY:-5}"

# The wrapper pins every platform version exactly, so a wrapper whose aliases
# do not match the payloads beside it would install aliases that 404. Check
# before anything is published.
shopt -s nullglob
platform_dirs=("${npm_dir%/}"/platforms/*/)
shopt -u nullglob
[ "${#platform_dirs[@]}" -gt 0 ] || fail "no platform payloads under ${npm_dir%/}/platforms"
expected_platforms="$(node -e 'const pkg = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8")); process.stdout.write(Object.values(pkg.optionalDependencies || {}).sort().join("\n"));' "${wrapper_dir}/package.json")"
actual_platforms=""
for dir in "${platform_dirs[@]}"; do
  dir="${dir%/}"
  platform_name="$(manifest_field "$dir" name)"
  platform_version="$(manifest_field "$dir" version)"
  [ "$platform_name" = "$name" ] || fail "${dir} is ${platform_name}, not ${name}"
  case "$platform_version" in
    "${version}-"*) ;;
    *) fail "${dir} version ${platform_version} is not a platform version of ${name}@${version}" ;;
  esac
  actual_platforms="${actual_platforms}npm:${name}@${platform_version}"$'\n'
done
actual_platforms="$(printf '%s' "$actual_platforms" | sort)"
if [ "$actual_platforms" != "$expected_platforms" ]; then
  fail "the wrapper's optionalDependencies do not match the platform payloads under ${npm_dir%/}/platforms (wrapper: $(echo "$expected_platforms" | tr '\n' ' '); payloads: $(echo "$actual_platforms" | tr '\n' ' '))"
fi

already_published() {
  [ "$(npm view "${name}@$1" version 2>/dev/null || true)" = "$1" ]
}

# publish <dir> <version> [npm publish flags...]
publish() {
  local dir="$1" package_version="$2"
  shift 2
  if already_published "$package_version"; then
    echo "${name}@${package_version} is already on the registry; not publishing it again."
    return 0
  fi
  echo "publishing ${name}@${package_version} from ${dir}"
  (cd "$dir" && npm publish --access public --ignore-scripts "$@")
}

for dir in "${platform_dirs[@]}"; do
  dir="${dir%/}"
  publish "$dir" "$(manifest_field "$dir" version)" --tag platform
done

# Platform versions are semver prereleases of the wrapper version, so a
# platform publish that took `latest` would make a plain install deliver a
# payload with no bin. Stop before the wrapper goes out. A failed lookup is
# tolerated (registry lag, or no `latest` yet on a package's first release):
# failing here would strand the platform payloads without their wrapper.
latest="$(npm view "$name" dist-tags.latest || true)"
case "$latest" in
  "${version}-"*)
    fail "dist-tag latest points at platform version ${latest}; a platform publish clobbered it. Fix the dist-tag before publishing the wrapper."
    ;;
esac
echo "dist-tag latest is ${latest:-unset} (unchanged by the platform publishes)."

publish "$wrapper_dir" "$version"

# Registry metadata can lag a publish by a few seconds.
for _ in 1 2 3 4 5; do
  latest="$(npm view "$name" dist-tags.latest || true)"
  if [ "$latest" = "$version" ]; then
    echo "dist-tag latest is ${version}."
    exit 0
  fi
  sleep "$delay"
done
fail "dist-tag latest of ${name} is ${latest:-unset}, expected ${version} after the wrapper publish."
