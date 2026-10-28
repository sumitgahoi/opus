#!/usr/bin/env bash
# Install, upgrade, or remove the Opus systemd service.
set -euo pipefail

repo=${OPUS_REPO:-sumitgahoi/music-server}
service=opus

die() {
  printf '%s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage: install.sh [vX.Y.Z]

Download the binary for this machine, install the systemd service, and
start it. When Opus is already installed, replace the binary and the
unit, and add config keys that are not set yet.

  vX.Y.Z   release tag; the default is the latest GitHub release

The music folder is stored in /etc/opus/opus.conf.
Uninstall with /usr/local/bin/uninstall.sh.
Set OPUS_RELEASE_BASE to a directory or URL of release assets
to install from somewhere other than GitHub.
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
  conf_dir=$prefix/etc/opus
  conf_path=$conf_dir/opus.conf
  state_dir=$prefix/var/lib/opus
}

prefixed() {
  printf '%s%s' "$prefix" "$1"
}

require_linux() {
  if [ "${OPUS_TEST:-}" = 1 ]; then
    return 0
  fi
  [ "$(uname -s)" = Linux ] || die "this installer supports Linux"
}

machine_arch() {
  local raw
  if [ "${OPUS_TEST:-}" = 1 ] && [ -n "${OPUS_ARCH:-}" ]; then
    raw=$OPUS_ARCH
  else
    raw=$(uname -m)
  fi
  case $raw in
    x86_64 | amd64) printf '%s' amd64 ;;
    aarch64 | arm64) printf '%s' arm64 ;;
    *) return 1 ;;
  esac
}

require_arch() {
  arch=$(machine_arch) || die "this installer supports linux/amd64 and linux/arm64 (this machine is $(uname -m))"
}

require_root() {
  if [ -n "$prefix" ]; then
    return 0
  fi
  [ "$(id -u)" -eq 0 ] || die "run as root: sudo ./install.sh $cmd"
  command -v systemctl >/dev/null || die "systemd is required"
}

run_systemctl() {
  if [ -n "$prefix" ]; then
    printf '%s\n' "$*" >>"$prefix/systemctl.log"
    return 0
  fi
  command systemctl "$@"
}

service_active() {
  if [ -n "$prefix" ]; then
    if [ -f "$prefix/service-active" ]; then
      return 0
    fi
    return 1
  fi
  command systemctl is-active --quiet "$service"
}

release_base() {
  local base=${OPUS_RELEASE_BASE:-}
  if [ -z "$base" ]; then
    if [ -n "$version" ]; then
      base="https://github.com/${repo}/releases/download/${version}"
    else
      base="https://github.com/${repo}/releases/latest/download"
    fi
  fi
  printf '%s' "${base%/}"
}

asset_url() {
  printf '%s/%s' "$(release_base)" "$1"
}

fetch() {
  local url=$1 dest=$2
  case $url in
    /* | file://*)
      local src=$url
      [ "${src#file://}" != "$src" ] && src=${src#file://}
      [ -f "$src" ] || die "release asset not found: $src"
      cp -- "$src" "$dest"
      ;;
    *)
      if [ "${OPUS_TEST:-}" = 1 ]; then
        die "test mode tried to download $url"
      fi
      command -v curl >/dev/null || die "curl is required to download the release"
      command curl -fsSL --retry 3 --retry-delay 1 -o "$dest" "$url"
      ;;
  esac
}

verify_sha256() {
  local file=$1 sumfile=$2 want got
  want=$(awk 'NR==1 { print $1 }' "$sumfile")
  got=$(sha256sum -- "$file" | awk '{ print $1 }')
  [ -n "$want" ] || die "checksum file is empty"
  [ "$want" = "$got" ] || die "checksum does not match"
}

trim() {
  local value=$1
  value=${value#"${value%%[![:space:]]*}"}
  value=${value%"${value##*[![:space:]]}"}
  printf '%s' "$value"
}

# key_name prints the setting name from a key=value line.
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

set_key() {
  local file=$1 key=$2 value=$3 tmp found=0 line name
  case $value in
    *$'\n'*) die "path contains a character the config file cannot store safely" ;;
  esac
  tmp=$(mktemp)
  if [ -f "$file" ]; then
    while IFS= read -r line || [ -n "$line" ]; do
      name=$(key_name "$line" || true)
      if [ "$name" = "$key" ]; then
        if [ "$found" -eq 0 ]; then
          printf '%s=%s\n' "$key" "$value" >>"$tmp"
          found=1
        fi
        continue
      fi
      printf '%s\n' "$line" >>"$tmp"
    done <"$file"
  fi
  if [ "$found" -eq 0 ]; then
    printf '%s=%s\n' "$key" "$value" >>"$tmp"
  fi
  mv -- "$tmp" "$file"
}

conf_source() {
  local dir src
  if [ -n "${OPUS_CONF_FILE:-}" ]; then
    printf '%s' "$OPUS_CONF_FILE"
    return 0
  fi
  dir=$(script_dir)
  for src in "$dir/deploy/opus.conf" "$dir/opus.conf"; do
    if [ -f "$src" ]; then
      printf '%s' "$src"
      return 0
    fi
  done
  return 1
}

# copy_template writes the shipped configuration to dest.
copy_template() {
  local dest=$1 src
  if src=$(conf_source); then
    cp -- "$src" "$dest"
    return 0
  fi
  fetch "$(asset_url opus.conf)" "$dest"
}

merge_template() {
  local file=$1 template line key
  template=$(mktemp)
  copy_template "$template"
  touch -- "$file"
  # A missing final newline would glue the next key onto the last line.
  if [ -s "$file" ] && [ "$(tail -c 1 -- "$file" | wc -l)" -eq 0 ]; then
    printf '\n' >>"$file"
  fi
  while IFS= read -r line || [ -n "$line" ]; do
    key=$(key_name "$line" || true)
    [ -n "$key" ] || continue
    if ! read_key "$key" "$file" >/dev/null; then
      printf '%s\n' "$line" >>"$file"
    fi
  done <"$template"
  rm -f -- "$template"
}

script_dir() {
  CDPATH= cd -- "$(dirname -- "$0")" && pwd
}

unit_source() {
  local dir src
  if [ -n "${OPUS_UNIT_FILE:-}" ]; then
    printf '%s' "$OPUS_UNIT_FILE"
    return 0
  fi
  dir=$(script_dir)
  for src in "$dir/deploy/opus.service" "$dir/opus.service"; do
    if [ -f "$src" ]; then
      printf '%s' "$src"
      return 0
    fi
  done
  return 1
}

uninstaller_source() {
  local dir src
  if [ -n "${OPUS_UNINSTALL_FILE:-}" ]; then
    printf '%s' "$OPUS_UNINSTALL_FILE"
    return 0
  fi
  dir=$(script_dir)
  src=$dir/uninstall.sh
  if [ -f "$src" ]; then
    printf '%s' "$src"
    return 0
  fi
  return 1
}

ensure_user() {
  install -d -m 0750 -- "$state_dir"
  if [ -n "$prefix" ]; then
    return 0
  fi
  if ! getent passwd "$service" >/dev/null; then
    command -v useradd >/dev/null || die "useradd is required"
    useradd --system --user-group --home-dir /var/lib/opus --shell /usr/sbin/nologin "$service"
  fi
  chown "$service:" "$state_dir"
}

ensure_readable() {
  local music=$1
  [ -d "$music" ] || die "music folder not found: $music"
  if [ -n "$prefix" ]; then
    return 0
  fi
  command -v runuser >/dev/null || die "runuser is required"
  if runuser -u "$service" -- test -r "$music" && runuser -u "$service" -- test -x "$music"; then
    return 0
  fi
  die "Opus cannot read ${music}. The service user needs read and search access on that folder and every directory above it."
}

prompt_music() {
  local music resolved
  printf 'Music folder: ' >&2
  if ! IFS= read -r music; then
    die "music folder is required"
  fi
  # A piped answer does not echo a newline after the prompt.
  if [ ! -t 0 ]; then
    printf '\n' >&2
  fi
  [ -n "$music" ] || die "music folder is required"
  if resolved=$(realpath -e -- "$music" 2>/dev/null); then
    :
  elif resolved=$(realpath -- "$music" 2>/dev/null); then
    :
  else
    die "music folder not found: $music"
  fi
  [ -d "$resolved" ] || die "$resolved is not a folder"
  ensure_readable "$resolved"
  printf '%s' "$resolved"
}

finish_config() {
  # BSD chmod does not accept --; paths here never start with -.
  chmod 640 "$conf_path"
  if [ -z "$prefix" ]; then
    chown "root:$service" "$conf_path"
  fi
}

ensure_config() {
  local ask=$1 existing music
  install -d -m 0755 -- "$conf_dir"
  if [ ! -f "$conf_path" ]; then
    copy_template "$conf_path"
    finish_config
  fi
  existing=$(read_key music_root "$conf_path" || true)
  if [ "$ask" -eq 1 ] && [ -z "$existing" ]; then
    music=$(prompt_music)
    set_key "$conf_path" music_root "$music"
    existing=$music
  fi
  [ -n "$existing" ] || die "music_root is not set in $conf_path"
  ensure_readable "$existing"
  merge_template "$conf_path"
  finish_config
}

install_binary() {
  local name=$1 work bin sum
  if [ "${OPUS_TEST:-}" = 1 ] && [ -z "${OPUS_RELEASE_BASE:-}" ]; then
    die "test mode requires OPUS_RELEASE_BASE"
  fi
  work=$(mktemp -d)
  bin=$work/opus
  sum=$work/opus.sha256
  # The subshell keeps a failing download from skipping this cleanup.
  if (
    fetch "$(asset_url "$name")" "$bin" &&
      fetch "$(asset_url "$name.sha256")" "$sum" &&
      verify_sha256 "$bin" "$sum" &&
      install -d -m 0755 -- "$(dirname -- "$bin_path")" &&
      install -m 0755 -- "$bin" "$bin_path"
  ); then
    rm -rf -- "$work"
  else
    rm -rf -- "$work"
    exit 1
  fi
}

install_unit() {
  local src tmp
  if src=$(unit_source); then
    :
  else
    tmp=$(mktemp)
    fetch "$(asset_url opus.service)" "$tmp"
    src=$tmp
  fi
  install -d -m 0755 -- "$(dirname -- "$unit_path")"
  install -m 0644 -- "$src" "$unit_path"
  if [ -n "${tmp:-}" ]; then
    rm -f -- "$tmp"
  fi
}

install_uninstaller() {
  local src tmp
  if src=$(uninstaller_source); then
    :
  else
    tmp=$(mktemp)
    fetch "$(asset_url uninstall.sh)" "$tmp"
    src=$tmp
  fi
  install -d -m 0755 -- "$(dirname -- "$uninstall_path")"
  install -m 0755 -- "$src" "$uninstall_path"
  if [ -n "${tmp:-}" ]; then
    rm -f -- "$tmp"
  fi
}

already_installed() {
  [ -x "$bin_path" ] && [ -f "$conf_path" ]
}

start_service() {
  run_systemctl daemon-reload
  run_systemctl enable --now "$service"
}

restart_if_active() {
  run_systemctl daemon-reload
  if service_active; then
    run_systemctl restart "$service"
    printf 'Restarted %s.\n' "$service"
  else
    printf 'Updated the unit. The service is not running.\n'
  fi
}

cmd_install() {
  require_arch
  printf 'Installing Opus for linux/%s.\n' "$arch"
  ensure_user
  ensure_config 1
  install_binary "opus-linux-${arch}"
  install_unit
  install_uninstaller
  start_service
  printf 'Installed %s and enabled %s.\n' "$bin_path" "$service"
  printf 'Music folder is in %s.\n' "$conf_path"
  printf 'Uninstall with %s.\n' "$uninstall_path"
}

cmd_upgrade() {
  require_arch
  printf 'Upgrading Opus for linux/%s.\n' "$arch"
  ensure_user
  ensure_config 0
  install_binary "opus-linux-${arch}"
  install_unit
  install_uninstaller
  restart_if_active
  printf 'Replaced %s and updated %s.\n' "$bin_path" "$conf_path"
}

main() {
  local arg
  version=${OPUS_VERSION:-}
  while [ "$#" -gt 0 ]; do
    arg=$1
    shift
    case $arg in
      -h | --help)
        usage
        exit 0
        ;;
      v*)
        [ -z "$version" ] || die "one version only"
        version=$arg
        ;;
      *)
        die "unknown argument: $arg"
        ;;
    esac
  done
  init_paths
  require_linux
  require_root
  if already_installed; then
    cmd_upgrade
  else
    cmd_install
  fi
}

main "$@"
