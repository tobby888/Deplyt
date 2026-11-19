set -euo pipefail
# maint-on/maint-off 定义在 ~/.bashrc 里，非交互 shell 不会加载，
# 所以用交互式 bash 调用；</dev/null 防止它吃掉本脚本的 stdin。
echo ">>> maint-on"
bash -ic 'maint-on' </dev/null

echo ">>> systemctl stop photolib"
systemctl stop photolib

echo ">>> 删除旧 JAR"
rm -fv "$DEPLOY_DIR"/photolib-backend*

echo ">>> 放入新 JAR: $JAR_NAME"
mv "$DEPLOY_DIR/.upload-$RUN_ID.jar" "$DEPLOY_DIR/$JAR_NAME"
# 上传是以 SSH 用户（默认 root）身份进行的，而服务以 photolib 用户运行；
# 显式改属主和权限，免得服务读不到 JAR。
chown photolib:photolib "$DEPLOY_DIR/$JAR_NAME"
chmod 644 "$DEPLOY_DIR/$JAR_NAME"

echo ">>> systemctl start photolib"
systemctl start photolib

sleep 10

echo ">>> maint-off"
bash -ic 'maint-off' </dev/null

if ! systemctl is-active --quiet photolib; then
  echo "photolib 服务启动后未处于 active 状态" >&2
  journalctl -u photolib -n 80 --no-pager >&2 || true
  exit 1
fi
systemctl status photolib --no-pager | head -n 5
