set -euo pipefail
shopt -s nullglob
cd "$DEPLOY_DIR"
artifact_format="${ARTIFACT_FORMAT:-jar}"
artifact_name="${ARTIFACT_NAME:-${JAR_NAME:-}}"
case "$artifact_format" in
  jar)
    staged=".upload-$RUN_ID.jar"
    ;;
  native)
    staged=".upload-$RUN_ID.tar.gz"
    ;;
  *)
    echo "不支持的部署制品类型: $artifact_format" >&2
    exit 1
    ;;
esac
if [[ -z "$artifact_name" ]]; then
  echo "缺少部署制品名称" >&2
  rm -f -- "$staged"
  exit 1
fi

deployment_id="${DEPLOY_ID:-${RUN_ID%-[0-9]*}}"
restore_db_from="${RESTORE_DB_FROM:-}"
pre_switch_db=".db-before-$RUN_ID.sql.gz"
db_snapshot_ready=0

dropin_dir="/etc/systemd/system/photolib.service.d"
dropin_file="$dropin_dir/zzz-deplyt-runtime.conf"
legacy_dropin_file="$dropin_dir/10-deplyt-runtime.conf"
native_releases_dir="$DEPLOY_DIR/native-releases"
native_current_link="$DEPLOY_DIR/native-current"

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

if [[ -n "$restore_db_from" && ! -f ".db-snapshot-$restore_db_from.sql.gz" ]]; then
  echo "未找到目标版本 ($restore_db_from) 的数据库快照，无法执行数据库同步回滚" >&2
  rm -f -- "$staged"
  exit 1
fi

prev_mode="jar"
prev_native_target=""
if [[ -f "$dropin_file" ]]; then
  prev_mode="native"
  if [[ -L "$native_current_link" ]]; then
    prev_native_target=$(readlink "$native_current_link" || true)
  fi
fi

read_env_var() {
  local key="$1" file="${2:-.env}"
  [[ -f "$file" ]] || return 1
  local line
  line=$(grep -E "^[[:space:]]*${key}=" "$file" | tail -n 1 || true)
  [[ -n "$line" ]] || return 1
  local val="${line#*=}"
  val="${val#"${val%%[![:space:]]*}"}"
  val="${val%"${val##*[![:space:]]}"}"
  if [[ "$val" =~ ^\'(.*)\'$ || "$val" =~ ^\"(.*)\"$ ]]; then
    val="${BASH_REMATCH[1]}"
  fi
  printf '%s' "$val"
}

parse_mysql_env() {
  DB_HOST="127.0.0.1"
  DB_PORT="3306"
  DB_NAME=""
  DB_USER=""
  DB_PASS=""
  local url
  url=$(read_env_var DB_URL || true)
  DB_USER=$(read_env_var DB_USERNAME || true)
  DB_PASS=$(read_env_var DB_PASSWORD || true)
  if [[ "$url" =~ ^jdbc:mysql://([^:/?]+)(:([0-9]+))?/([^?]+) ]]; then
    DB_HOST="${BASH_REMATCH[1]}"
    DB_PORT="${BASH_REMATCH[3]:-3306}"
    DB_NAME="${BASH_REMATCH[4]}"
  fi
  [[ -n "$DB_NAME" && -n "$DB_USER" ]]
}

snapshot_database() {
  local target_file="$1"
  if ! command -v mysqldump >/dev/null 2>&1 || ! parse_mysql_env; then
    return 1
  fi
  local cnf
  cnf=$(mktemp "$DEPLOY_DIR/.my-cnf-XXXXXX")
  chmod 600 "$cnf"
  printf '[client]\nhost=%s\nport=%s\nuser=%s\npassword=%s\n' "$DB_HOST" "$DB_PORT" "$DB_USER" "$DB_PASS" >"$cnf"
  if mysqldump --defaults-extra-file="$cnf" --no-tablespaces --single-transaction --routines --triggers --hex-blob "$DB_NAME" | gzip -c >"$target_file"; then
    rm -f -- "$cnf"
    chmod 600 "$target_file"
    return 0
  fi
  rm -f -- "$cnf" "$target_file"
  return 1
}

restore_database() {
  local source_file="$1"
  [[ -f "$source_file" ]] || return 1
  if ! command -v mysql >/dev/null 2>&1 || ! parse_mysql_env; then
    return 1
  fi
  local cnf
  cnf=$(mktemp "$DEPLOY_DIR/.my-cnf-XXXXXX")
  chmod 600 "$cnf"
  printf '[client]\nhost=%s\nport=%s\nuser=%s\npassword=%s\n' "$DB_HOST" "$DB_PORT" "$DB_USER" "$DB_PASS" >"$cnf"
  if gzip -dc "$source_file" | mysql --defaults-extra-file="$cnf" "$DB_NAME"; then
    rm -f -- "$cnf"
    return 0
  fi
  rm -f -- "$cnf"
  return 1
}

# Keep the previous JAR and systemd drop-in outside the active paths until the new service starts.
backup=".previous-$RUN_ID"
backup_dropin=".dropin-backup-$RUN_ID.conf"
new_native_dir=""
tmp_dropin=""
maintenance=0
switching=0
activated=0
finish() {
  local status=$?
  trap - EXIT
  set +e
  if (( switching && ! activated )); then
    systemctl stop photolib
    if (( db_snapshot_ready )) && [[ -f "$pre_switch_db" ]]; then
      restore_database "$pre_switch_db" || echo "警告：回滚发版前数据库快照失败" >&2
    fi
    if [[ "$prev_mode" == "native" ]]; then
      if [[ "$artifact_format" == "jar" ]]; then
        rm -f -- "$artifact_name"
      fi
      if [[ -f "$backup" ]] && (( ${#previous[@]} )); then
        mv -f -- "$backup" "${previous[0]}"
      fi
      if [[ -f "$backup_dropin" ]]; then
        mkdir -p "$dropin_dir"
        cp -f -- "$backup_dropin" "$dropin_file"
      fi
      if [[ -n "$prev_native_target" ]]; then
        ln -sfn "$prev_native_target" "$native_current_link"
        chown -h photolib:photolib "$native_current_link"
      fi
      systemctl daemon-reload
      systemctl start photolib
    else
      rm -f -- "$dropin_file"
      if [[ -z "$prev_native_target" ]]; then
        rm -f -- "$native_current_link"
      fi
      if [[ -f "$backup" ]]; then
        if [[ "$artifact_format" == "jar" && "$artifact_name" != "${previous[0]}" ]]; then
          rm -f -- "$artifact_name"
        fi
        mv -f -- "$backup" "${previous[0]}" && systemctl daemon-reload && systemctl start photolib
      elif (( ${#previous[@]} )); then
        systemctl daemon-reload && systemctl start photolib
      else
        if [[ "$artifact_format" == "jar" ]]; then
          rm -f -- "$artifact_name"
        fi
      fi
    fi
  fi
  if (( ! activated )) && [[ -n "$new_native_dir" ]]; then
    rm -rf -- "$new_native_dir"
  fi
  rm -f -- "$staged" "$pre_switch_db" "$backup_dropin" ${tmp_dropin:+"$tmp_dropin"}
  if (( maintenance )); then
    bash -ic 'maint-off' </dev/null || status=1
  fi
  exit "$status"
}
trap finish EXIT
trap 'exit 1' HUP INT TERM

if [[ "$artifact_format" == "native" ]]; then
  mkdir -p "$native_releases_dir"
  chown photolib:photolib "$native_releases_dir"
  chmod 755 "$native_releases_dir"
  new_native_dir="$native_releases_dir/$RUN_ID"
  rm -rf -- "$new_native_dir"
  mkdir -p "$new_native_dir"
  tar -xzf "$staged" --no-same-owner --no-same-permissions -C "$new_native_dir"
  if [[ ! -f "$new_native_dir/photolib" || -L "$new_native_dir/photolib" || ! -s "$new_native_dir/photolib" ]]; then
    echo "原生发布包缺少有效的 photolib 可执行文件" >&2
    exit 1
  fi
  chown -R photolib:photolib "$new_native_dir"
  chmod 755 "$new_native_dir"
  chmod 644 "$new_native_dir"/*.so
  chmod 755 "$new_native_dir/photolib"
else
  chown photolib:photolib "$staged"
  chmod 644 "$staged"
fi

# These commands are defined in the SSH user's interactive bash configuration.
maintenance=1
bash -ic 'maint-on' </dev/null
systemctl stop photolib
switching=1

if snapshot_database "$pre_switch_db"; then
  db_snapshot_ready=1
fi

if [[ -n "$restore_db_from" ]]; then
  if (( db_snapshot_ready )); then
    cp -f -- "$pre_switch_db" ".db-before-rollback-$RUN_ID.sql.gz"
  fi
  if ! restore_database ".db-snapshot-$restore_db_from.sql.gz"; then
    echo "恢复目标版本 ($restore_db_from) 的数据库快照失败" >&2
    exit 1
  fi
fi

if (( ${#previous[@]} )); then
  mv -- "${previous[0]}" "$backup"
fi
if [[ -f "$dropin_file" ]]; then
  cp -p -- "$dropin_file" "$backup_dropin"
fi

if [[ "$artifact_format" == "native" ]]; then
  ln -sfn "native-releases/$RUN_ID" "$native_current_link"
  chown -h photolib:photolib "$native_current_link"
  mkdir -p "$dropin_dir"
  tmp_dropin=$(mktemp "$dropin_dir/.zzz-deplyt-runtime.XXXXXX")
  chmod 644 "$tmp_dropin"
  printf '[Service]\nExecStart=\nExecStart=%s/native-current/photolib\n' "$DEPLOY_DIR" >"$tmp_dropin"
  mv -f -- "$tmp_dropin" "$dropin_file"
  rm -f -- "$legacy_dropin_file"
  tmp_dropin=""
  systemctl daemon-reload
  systemctl start photolib
else
  if [[ -f "$dropin_file" || -f "$legacy_dropin_file" ]]; then
    rm -f -- "$dropin_file" "$legacy_dropin_file"
    systemctl daemon-reload
  fi
  mv -- "$staged" "$artifact_name"
  systemctl start photolib
fi

sleep 10
if ! systemctl is-active --quiet photolib; then
  journalctl -u photolib -n 80 --no-pager >&2 || true
  exit 1
fi
activated=1
if (( db_snapshot_ready )) && [[ -f "$pre_switch_db" ]]; then
  mv -f -- "$pre_switch_db" ".db-snapshot-$deployment_id.sql.gz"
fi
rm -f -- "$backup" "$backup_dropin"
if [[ "$artifact_format" == "native" ]]; then
  for old_dir in "$native_releases_dir"/*; do
    if [[ "$old_dir" != "$new_native_dir" ]]; then
      rm -rf -- "$old_dir"
    fi
  done
else
  rm -f -- "$native_current_link"
  rm -rf -- "$native_releases_dir"
fi
