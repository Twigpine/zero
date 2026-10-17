#!/usr/bin/env bash
# publish-packages.sh: publish the npm payloads that build-platform-packages.mjs
# assembled under <npm-dir> (wrapper/ plus platforms/*/), in the order and
# with the dist-tags docs/NPM_PACKAGING.md requires:
#
#   1. every platform payload, under the `platform` dist-tag, never `latest`;
#   2. wait until npm shows every platform version, then check that `latest`
#      was not moved onto one;
#   3. the wrapper, which takes `latest`, then wait until `latest` is its
#      version.
#
# The waits exist because npm applies a publish asynchronously: `npm publish`
# returns once the upload is accepted, and the version can appear minutes
# later (ten minutes for a platform payload of 0.10.0). Ordering the uploads
# alone would still let the wrapper appear first, with aliases that 404.
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
# ZERO_NPM_PUBLISH_RETRY_DELAY overrides the seconds between registry lookups,
# and ZERO_NPM_PUBLISH_WAIT_TIMEOUT the seconds to wait for npm to apply the
# publishes (default 1800).
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
# whole_seconds <variable> <value> <max>: the value as a decimal number of
# seconds. Checked here, before anything is published: Bash arithmetic reads a
# leading zero as octal (0600 would wait 384 seconds, and 08 is an error), a
# negative timeout would end the wait before it starts, and a bad delay would
# only fail at the first sleep, possibly after the platform payloads went out.
whole_seconds() {
  case "$2" in
    '' | *[!0-9]*) fail "$1 must be a whole number of seconds, got '$2'" ;;
  esac
  # Bounded before the conversion so it cannot overflow.
  [ "${#2}" -le 18 ] || fail "$1 must be at most $3 seconds, got '$2'"
  local seconds=$((10#$2))
  [ "$seconds" -le "$3" ] || fail "$1 must be at most $3 seconds, got '$2'"
  echo "$seconds"
}

delay="$(whole_seconds ZERO_NPM_PUBLISH_RETRY_DELAY "${ZERO_NPM_PUBLISH_RETRY_DELAY:-5}" 600)"
wait_timeout="$(whole_seconds ZERO_NPM_PUBLISH_WAIT_TIMEOUT "${ZERO_NPM_PUBLISH_WAIT_TIMEOUT:-1800}" 86400)"

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

# npm reports every kind of "not on the registry" (no such package, no such
# version) as E404. Any other lookup failure (network, auth, a registry
# outage) is retried and then fails the job: reading it as "not published"
# would publish a version that may already be out, and npm would reject that
# with EPUBLISHCONFLICT on the very re-run this skip exists for.
already_published() {
  local output status errors
  errors="$(mktemp)"
  for _ in 1 2 3; do
    status=0
    output="$(npm view "${name}@$1" version 2>"$errors")" || status=$?
    if [ "$status" -eq 0 ]; then
      rm -f "$errors"
      [ "$output" = "$1" ]
      return
    fi
    if grep -q 'code E404' "$errors"; then
      rm -f "$errors"
      return 1
    fi
    sleep "$delay"
  done
  cat "$errors" >&2
  rm -f "$errors"
  fail "could not look up ${name}@$1 on the registry, so cannot tell whether it is already published"
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

# Unlike already_published, these lookups may fail without stopping the job:
# while npm has not applied a publish, an error or an E404 both mean "not
# yet", and the deadline bounds the wait.
version_visible() {
  [ "$(npm view "${name}@$1" version 2>/dev/null || true)" = "$1" ]
}

latest_is() {
  [ "$(npm view "$name" dist-tags.latest 2>/dev/null || true)" = "$1" ]
}

# One deadline covers every wait in this run, so the job cannot wait
# wait_timeout once per version.
deadline=$((SECONDS + wait_timeout))

# wait_until <what we are waiting for> <command...>
wait_until() {
  local what="$1"
  shift
  until "$@"; do
    if [ "$SECONDS" -ge "$deadline" ]; then
      fail "npm accepted the upload but still does not show ${what} after ${wait_timeout}s. Re-run once it does: versions already on the registry are skipped."
    fi
    sleep "$delay"
  done
}

for dir in "${platform_dirs[@]}"; do
  dir="${dir%/}"
  publish "$dir" "$(manifest_field "$dir" version)" --tag platform
done

for dir in "${platform_dirs[@]}"; do
  platform_version="$(manifest_field "${dir%/}" version)"
  wait_until "${name}@${platform_version}" version_visible "$platform_version"
  echo "${name}@${platform_version} is on the registry."
done

# Platform versions are semver prereleases of the wrapper version, so a
# platform publish that took `latest` would make a plain install deliver a
# payload with no bin. Stop before the wrapper goes out. A failed lookup is
# tolerated (no `latest` yet on a package's first release): failing here would
# strand the platform payloads without their wrapper.
latest="$(npm view "$name" dist-tags.latest || true)"
case "$latest" in
  "${version}-"*)
    fail "dist-tag latest points at platform version ${latest}; a platform publish clobbered it. Fix the dist-tag before publishing the wrapper."
    ;;
esac
echo "dist-tag latest is ${latest:-unset} (unchanged by the platform publishes)."

publish "$wrapper_dir" "$version"

wait_until "${name}@${version} as the latest dist-tag" latest_is "$version"
echo "dist-tag latest is ${version}."
