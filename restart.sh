set -euo pipefail
shopt -s nullglob
cd "$DEPLOY_DIR"
staged=".upload-$RUN_ID.jar"
deployment_id="${DEPLOY_ID:-${RUN_ID%-[0-9]*}}"
restore_db_from="${RESTORE_DB_FROM:-}"
pre_switch_db=".db-before-$RUN_ID.sql.gz"
db_snapshot_ready=0

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
  if mysqldump --defaults-extra-file="$cnf" --single-transaction --routines --triggers --hex-blob "$DB_NAME" | gzip -c >"$target_file"; then
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
    if (( db_snapshot_ready )) && [[ -f "$pre_switch_db" ]]; then
      restore_database "$pre_switch_db" || echo "警告：回滚发版前数据库快照失败" >&2
    fi
    if [[ -f "$backup" ]]; then
      [[ "$JAR_NAME" == "${previous[0]}" ]] || rm -f -- "$JAR_NAME"
      mv -f -- "$backup" "${previous[0]}" && systemctl start photolib
    elif (( ${#previous[@]} )); then
      systemctl start photolib
    else
      rm -f -- "$JAR_NAME"
    fi
  fi
  rm -f -- "$staged" "$pre_switch_db"
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
mv -- "$staged" "$JAR_NAME"
systemctl start photolib
sleep 10
if ! systemctl is-active --quiet photolib; then
  journalctl -u photolib -n 80 --no-pager >&2 || true
  exit 1
fi
activated=1
if (( db_snapshot_ready )) && [[ -f "$pre_switch_db" ]]; then
  mv -f -- "$pre_switch_db" ".db-snapshot-$deployment_id.sql.gz"
fi
rm -f -- "$backup"
