#!/usr/bin/env bash
# Remove the Opus service and binary. Optionally remove its database and artwork.
set -euo pipefail

service=opus
default_db=/var/lib/opus/inventory.db

die() {
  printf '%s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage: uninstall.sh [--keep-data|--purge]

  --keep-data   remove the service and binary; leave the database and artwork
  --purge       remove the service, binary, database, and artwork

With neither flag, uninstall.sh asks which one to do.
The configuration file and the music folder stay.
EOF
}

init_paths() {
  prefix=
  if [ "${OPUS_TEST:-}" = 1 ]; then
    [ -n "${OPUS_PREFIX:-}" ] || die "test mode requires OPUS_PREFIX"
    prefix=${OPUS_PREFIX%/}
  fi
  bin_path=$prefix/usr/local/bin/opus
  uninstall_path=$prefix/usr/local/bin/uninstall.sh
  unit_path=$prefix/etc/systemd/system/opus.service
  conf_path=$prefix/etc/opus/opus.conf
}

prefixed() {
  printf '%s%s' "$prefix" "$1"
}

require_linux() {
  if [ "${OPUS_TEST:-}" = 1 ]; then
    return 0
  fi
  [ "$(uname -s)" = Linux ] || die "this uninstaller supports Linux"
}

require_root() {
  if [ -n "$prefix" ]; then
    return 0
  fi
  [ "$(id -u)" -eq 0 ] || die "run as root: sudo /usr/local/bin/uninstall.sh"
  command -v systemctl >/dev/null || die "systemd is required"
}

run_systemctl() {
  if [ -n "$prefix" ]; then
    printf '%s\n' "$*" >>"$prefix/systemctl.log"
    return 0
  fi
  command systemctl "$@"
}

trim() {
  local value=$1
  value=${value#"${value%%[![:space:]]*}"}
  value=${value%"${value##*[![:space:]]}"}
  printf '%s' "$value"
}

key_name() {
  local line name
  line=$(trim "$1")
  case $line in
    '' | \#*) return 1 ;;
    *=*) ;;
    *) return 1 ;;
  esac
  name=$(trim "${line%%=*}")
  [ -n "$name" ] || return 1
  printf '%s' "$name"
}

read_key() {
  local key=$1 file=$2 line name value
  [ -f "$file" ] || return 1
  while IFS= read -r line || [ -n "$line" ]; do
    name=$(key_name "$line" || true)
    [ "$name" = "$key" ] || continue
    value=$(trim "${line#*=}")
    printf '%s' "$value"
    return 0
  done <"$file"
  return 1
}

# safe_db accepts the inventory file this installer created. Other paths
# are refused so purge cannot delete the music folder or an arbitrary file.
safe_db() {
  local db=$1 music=$2
  db=${db%/}
  case $db in
    /var/lib/opus/*) ;;
    *) return 1 ;;
  esac
  case $db in
    *..*) return 1 ;;
  esac
  if [ -n "$music" ]; then
    case $db in
      "$music" | "$music"/*) return 1 ;;
    esac
    case $music in
      "$db" | "$db"/*) return 1 ;;
    esac
  fi
  printf '%s' "$db"
}

safe_cache() {
  local db=$1 music=$2 cache
  db=${db%/}
  cache=${db}.artwork
  case $cache in
    /var/lib/opus/*.artwork) ;;
    *) return 1 ;;
  esac
  case $cache in
    *..*) return 1 ;;
  esac
  if [ -n "$music" ] && { [ "$cache" = "$music" ] || [ "$db" = "$music" ]; }; then
    return 1
  fi
  printf '%s' "$cache"
}

prepare_purge() {
  local db music cache target
  db=$default_db
  music=$(read_key music_root "$conf_path" || true)
  db=$(safe_db "$db" "$music") || die "the database path is not one this installer will delete"
  cache=$(safe_cache "$db" "$music") || die "the artwork cache path is not one this installer will delete"
  target=$(prefixed "$db")
  if [ -d "$target" ] && [ ! -L "$target" ]; then
    die "database path is a directory: $target"
  fi
  purge_db=$target
  purge_cache=$(prefixed "$cache")
}

choose_mode() {
  local reply
  if [ -n "$uninstall_mode" ]; then
    return 0
  fi
  if [ ! -t 0 ]; then
    die "pass --keep-data to leave the database and artwork, or --purge to remove them"
  fi
  printf 'keep    remove the service and binary; leave the database and artwork\n' >&2
  printf 'purge   remove the service, binary, database, and artwork\n' >&2
  printf 'Choice [keep/purge]: ' >&2
  if ! IFS= read -r reply; then
    die "cancelled"
  fi
  case $reply in
    keep) uninstall_mode=keep ;;
    purge) uninstall_mode=purge ;;
    *) die "cancelled" ;;
  esac
}

remove_service_and_binary() {
  if [ -z "$prefix" ]; then
    run_systemctl disable --now "$service" || true
    run_systemctl reset-failed "$service" || true
  else
    run_systemctl disable --now "$service"
  fi
  rm -f -- "$unit_path"
  run_systemctl daemon-reload || true
  rm -f -- "$bin_path" "$uninstall_path"
}

main() {
  local arg
  uninstall_mode=
  while [ "$#" -gt 0 ]; do
    arg=$1
    shift
    case $arg in
      -h | --help)
        usage
        exit 0
        ;;
      --keep-data)
        [ -z "$uninstall_mode" ] || [ "$uninstall_mode" = keep ] || die "one uninstall choice only"
        uninstall_mode=keep
        ;;
      --purge)
        [ -z "$uninstall_mode" ] || [ "$uninstall_mode" = purge ] || die "one uninstall choice only"
        uninstall_mode=purge
        ;;
      *)
        die "unknown argument: $arg"
        ;;
    esac
  done
  init_paths
  require_linux
  require_root
  choose_mode
  if [ "$uninstall_mode" = purge ]; then
    prepare_purge
  fi
  remove_service_and_binary
  if [ "$uninstall_mode" = purge ]; then
    rm -f -- "$purge_db" "${purge_db}-wal" "${purge_db}-shm"
    if [ -e "$purge_cache" ] || [ -L "$purge_cache" ]; then
      rm -rf -- "$purge_cache"
    fi
    printf 'Removed the service, the binary, the database, and the artwork cache.\n'
    printf 'Kept %s and the music folder.\n' "$conf_path"
    return 0
  fi
  printf 'Removed the service and the binary.\n'
  printf 'Kept the database, the artwork cache, %s, and the music folder.\n' "$conf_path"
}

main "$@"
