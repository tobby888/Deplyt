set -euo pipefail
shopt -s nullglob
cd "$DEPLOY_DIR"
staged=".upload-$RUN_ID.jar"
exec 9>.deplyt-switch.lock
if ! flock -n 9; then
  rm -f -- "$staged"
  echo "PhotoLib 版本切换正在进行" >&2
  exit 1
fi
previous=(photolib-backend-*.jar)
if (( ${#previous[@]} > 1 )); then
  echo "生产目录有多个匹配的 JAR，无法确认现行版本" >&2
  rm -f -- "$staged"
  exit 1
fi

# Keep the previous JAR outside the service's glob until the new one starts.
backup=".previous-$RUN_ID"
maintenance=0
switching=0
activated=0
finish() {
  local status=$?
  trap - EXIT
  set +e
  if (( switching && ! activated )); then
    systemctl stop photolib
    if [[ -f "$backup" ]]; then
      [[ "$JAR_NAME" == "${previous[0]}" ]] || rm -f -- "$JAR_NAME"
      mv -f -- "$backup" "${previous[0]}" && systemctl start photolib
    elif (( ${#previous[@]} )); then
      systemctl start photolib
    else
      rm -f -- "$JAR_NAME"
    fi
  fi
  rm -f -- "$staged"
  if (( maintenance )); then
    bash -ic 'maint-off' </dev/null || status=1
  fi
  exit "$status"
}
trap finish EXIT
trap 'exit 1' HUP INT TERM

chown photolib:photolib "$staged"
chmod 644 "$staged"
# These commands are defined in the SSH user's interactive bash configuration.
maintenance=1
bash -ic 'maint-on' </dev/null
systemctl stop photolib
switching=1
if (( ${#previous[@]} )); then
  mv -- "${previous[0]}" "$backup"
fi
mv -- "$staged" "$JAR_NAME"
systemctl start photolib
sleep 10
if ! systemctl is-active --quiet photolib; then
  journalctl -u photolib -n 80 --no-pager >&2 || true
  exit 1
fi
activated=1
rm -f -- "$backup"
