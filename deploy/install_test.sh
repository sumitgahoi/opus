#!/usr/bin/env bash
# Exercises install.sh without root, systemd, or the network.
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
installer=$root/install.sh
uninstaller=$root/uninstall.sh

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_file() {
  [ -f "$1" ] || fail "missing file $1"
}

assert_dir() {
  [ -d "$1" ] || fail "missing directory $1"
}

assert_absent() {
  if [ -e "$1" ] || [ -L "$1" ]; then
    fail "should not exist: $1"
  fi
}

new_release() {
  local rel=$1 body=$2
  mkdir -p -- "$rel"
  printf '%s' "$body" >"$rel/opus-linux-amd64"
  sha256sum -- "$rel/opus-linux-amd64" >"$rel/opus-linux-amd64.sha256"
}

run_at() {
  local prefix=$1
  shift
  OPUS_TEST=1 \
    OPUS_PREFIX="$prefix" \
    OPUS_RELEASE_BASE="$rel" \
    OPUS_ARCH="${OPUS_ARCH:-amd64}" \
    "$installer" "$@"
}

run_uninstall() {
  local prefix=$1
  shift
  OPUS_TEST=1 \
    OPUS_PREFIX="$prefix" \
    "$uninstaller" "$@"
}

expect_fail() {
  local status=0
  "$@" >/dev/null 2>"$tmp/stderr" || status=$?
  [ "$status" -ne 0 ] || fail "expected failure: $*"
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Help does not need root or a release.
"$installer" --help >/dev/null

expect_fail "$installer" nosuch

# Unsupported architecture fails before writing anything.
prefix=$tmp/arch
mkdir -p -- "$prefix"
rel=$tmp/empty-release
mkdir -p -- "$rel"
OPUS_ARCH=ppc64le expect_fail run_at "$prefix" </dev/null
assert_absent "$prefix/usr/local/bin/opus"
assert_absent "$prefix/etc/opus/opus.conf"

# A bad checksum does not install the binary.
prefix=$tmp/checksum
rel=$tmp/bad-release
new_release "$rel" 'binary-v1'
printf '0000000000000000000000000000000000000000000000000000000000000000  opus-linux-amd64\n' >"$rel/opus-linux-amd64.sha256"
music=$tmp/music-checksum
mkdir -p -- "$music"
OPUS_ARCH=amd64 expect_fail run_at "$prefix" <<<"$music"
assert_absent "$prefix/usr/local/bin/opus"
assert_absent "$prefix/etc/systemd/system/opus.service"
# The music folder was already saved, so the retry must not ask again.
sha256sum -- "$rel/opus-linux-amd64" >"$rel/opus-linux-amd64.sha256"
run_at "$prefix" </dev/null >"$tmp/retry.out"
assert_file "$prefix/usr/local/bin/opus"

# Install, upgrade, and uninstall on a throwaway prefix.
prefix=$tmp/ok
rel=$tmp/release
music=$tmp/my\ music
mkdir -p -- "$music"
new_release "$rel" 'binary-v1'
OPUS_ARCH=amd64 run_at "$prefix" <<<"$music" >"$tmp/install.out"

bin=$prefix/usr/local/bin/opus
removed=$prefix/usr/local/bin/uninstall.sh
unit=$prefix/etc/systemd/system/opus.service
conf=$prefix/etc/opus/opus.conf
db=$prefix/var/lib/opus/inventory.db
cache=$prefix/var/lib/opus/inventory.db.artwork

assert_file "$bin"
assert_file "$removed"
assert_file "$unit"
assert_file "$conf"
cmp -s "$bin" "$rel/opus-linux-amd64" || fail "installed binary does not match the release"
cmp -s "$removed" "$uninstaller" || fail "uninstaller was not installed next to the binary"
diff -q "$unit" "$root/deploy/opus.service" >/dev/null || fail "unit was not installed from the repository"
grep -q 'Restart=on-failure' "$unit" || fail "unit does not restart on failure"
grep -q 'WantedBy=multi-user.target' "$unit" || fail "unit does not start at boot"
grep -q 'AmbientCapabilities=CAP_NET_BIND_SERVICE' "$unit" || fail "unit cannot bind port 80"
stored_root=$(awk -F= '/^music_root=/ { print substr($0, index($0, "=") + 1); exit }' "$conf")
case $stored_root in
  "$music" | "$(cd -- "$music" && pwd -P)") ;;
  *) fail "music folder was not stored: $(cat "$conf")" ;;
esac
[ "$(grep -c '^music_root=' "$conf")" -eq 1 ] || fail "music_root was written more than once"
if grep -q '^MUSIC_' "$conf"; then
  fail "config still uses environment assignments"
fi
conf_mode=$(stat -c %a "$conf" 2>/dev/null || stat -f %Lp "$conf")
[ "$conf_mode" = 640 ] || fail "config mode is $conf_mode"
grep -q 'enable --now opus' "$prefix/systemctl.log" || fail "service was not enabled"
grep -q 'Installing Opus for linux/amd64.' "$tmp/install.out" || fail "install did not report the architecture"

# A second run sees the existing server and upgrades without asking again.
run_at "$prefix" </dev/null >"$tmp/second.out"
grep -q 'Upgrading Opus for linux/amd64.' "$tmp/second.out" || fail "second run did not upgrade"
cmp -s "$bin" "$rel/opus-linux-amd64" || fail "second run changed the binary"

# Upgrade keeps edited values, adds a key from the new default, and replaces the binary.
printf 'note=kept\n' >>"$conf"
printf '# kept comment' >>"$conf"
printf 'music_root=\nallowed_users=\n' >"$tmp/next.conf"
new_release "$rel" 'binary-v2'
: >"$prefix/systemctl.log"
OPUS_ARCH=amd64 OPUS_CONF_FILE=$tmp/next.conf run_at "$prefix" >"$tmp/upgrade-stopped.out"
grep -q 'not running' "$tmp/upgrade-stopped.out" || fail "inactive upgrade restarted or hid its state"
if grep -q '^restart ' "$prefix/systemctl.log"; then
  fail "inactive service was restarted"
fi
grep -q '^note=kept$' "$conf" || fail "upgrade dropped an extra setting"
grep -q '^# kept comment$' "$conf" || fail "upgrade dropped a comment"
grep -q '^allowed_users=$' "$conf" || fail "upgrade did not add the new setting"
[ "$(grep -c '^music_root=' "$conf")" -eq 1 ] || fail "upgrade duplicated music_root"
stored_root=$(awk -F= '/^music_root=/ { print substr($0, index($0, "=") + 1); exit }' "$conf")
case $stored_root in
  "$music" | "$(cd -- "$music" && pwd -P)") ;;
  *) fail "upgrade changed the music folder" ;;
esac
cmp -s "$bin" "$rel/opus-linux-amd64" || fail "upgrade did not replace the binary"

touch -- "$prefix/service-active"
new_release "$rel" 'binary-v3'
: >"$prefix/systemctl.log"
OPUS_ARCH=amd64 run_at "$prefix" >"$tmp/upgrade-active.out"
grep -q '^restart opus$' "$prefix/systemctl.log" || fail "running service was not restarted"
grep -q 'Restarted opus.' "$tmp/upgrade-active.out" || fail "restart was not reported"
cmp -s "$bin" "$rel/opus-linux-amd64" || fail "second upgrade did not replace the binary"

# Without a choice, uninstall removes nothing.
printf 'db' >"$db"
printf 'wal' >"${db}-wal"
printf 'shm' >"${db}-shm"
mkdir -p -- "$cache"
printf 'cover' >"$cache/cover"
expect_fail run_uninstall "$prefix" </dev/null
expect_fail run_uninstall "$prefix" -y </dev/null
assert_file "$bin"
assert_file "$cache/cover"

# --keep-data removes the service and binary and leaves the database and artwork.
: >"$prefix/systemctl.log"
run_uninstall "$prefix" --keep-data >"$tmp/uninstall.out"
assert_absent "$bin"
assert_absent "$removed"
assert_absent "$unit"
assert_file "$db"
assert_file "${db}-wal"
assert_file "$cache/cover"
assert_file "$conf"
assert_dir "$music"
grep -q 'Kept the database, the artwork cache' "$tmp/uninstall.out" || fail "keep-data did not say what it left"
grep -q 'disable --now opus' "$prefix/systemctl.log" || fail "service was not disabled"

# --purge removes the database, its sidecars, and the artwork cache.
prefix=$tmp/purge
rel=$tmp/release
new_release "$rel" 'binary-v1'
run_at "$prefix" <<<"$music" >/dev/null
purge_bin=$prefix/usr/local/bin/opus
purge_unit=$prefix/etc/systemd/system/opus.service
purge_db=$prefix/var/lib/opus/inventory.db
purge_cache=$prefix/var/lib/opus/inventory.db.artwork
printf 'db' >"$purge_db"
printf 'wal' >"${purge_db}-wal"
printf 'shm' >"${purge_db}-shm"
mkdir -p -- "$purge_cache"
printf 'cover' >"$purge_cache/cover"
run_uninstall "$prefix" --purge >"$tmp/purge.out"
assert_absent "$purge_bin"
assert_absent "$prefix/usr/local/bin/uninstall.sh"
assert_absent "$purge_unit"
assert_absent "$purge_db"
assert_absent "${purge_db}-wal"
assert_absent "${purge_db}-shm"
assert_absent "$purge_cache"
assert_file "$prefix/etc/opus/opus.conf"
assert_dir "$music"
grep -q 'Removed the service, the binary, the database, and the artwork cache.' "$tmp/purge.out" || fail "purge did not report what it removed"

# A database path outside the data directory is refused before anything is removed.
prefix=$tmp/unsafe
new_release "$rel" 'binary-v1'
run_at "$prefix" <<<"$music" >/dev/null
unsafe_bin=$prefix/usr/local/bin/opus
unsafe_unit=$prefix/etc/systemd/system/opus.service
unsafe_cache=$prefix/var/lib/opus/inventory.db.artwork
mkdir -p -- "$unsafe_cache"
printf 'cover' >"$unsafe_cache/cover"
# Point music_root at the state dir so safe_db refuses (music overlaps db).
sed -i.bak 's|^music_root=.*|music_root=/var/lib/opus|' "$prefix/etc/opus/opus.conf"
expect_fail run_uninstall "$prefix" --purge
assert_file "$unsafe_bin"
assert_file "$unsafe_unit"
assert_dir "$unsafe_cache"
grep -q 'not one this installer will delete' "$tmp/stderr" || fail "unsafe purge was not refused"

printf 'ok\n'
