# 常见问题

## 1. 日志解析出的时间不正确
这是时区问题。本项目使用系统时区进行解析与统计，请确保运行环境时区正确。
- Docker: 挂载 `/etc/localtime:/etc/localtime:ro`
- 裸机: 设置系统时区后重启服务

## 2. 接口 500 或无法访问
- 检查 PostgreSQL 是否可连接（DSN 是否正确）。
- 检查端口占用（8088/8089）。
- 查看 `var/nginxpulse_data/nginxpulse.log` 获取错误信息。

## 3. 数据为空或统计不完整
- 解析尚未完成，观察页面“解析中”提示或 `/api/status`。
- 时间范围选择过小或过滤规则过严。
- 日志文件路径错误或无读权限（参考《Deployment》中的“Docker 部署权限说明”）。

## 4. 重启后重复解析
- 确认没有残留进程占用端口（避免多个实例同时解析）。
- 确认日志文件是否被轮转或替换。
- 确认 `websites[].name` 未被修改。

## 5. 迁移弹窗总是出现
- 当检测到 `var/nginxpulse_data/nginxpulse.db` 且没有 `pg_migration_done` 时会提示。
- 完成迁移后会生成 `pg_migration_done`，后续不会再弹。

## 6. 本地开发数据是否持久化
`dev_local.sh` 默认使用 docker volume `nginxpulse_pgdata`，不会自动删除。
需要重置时请手动删除该 volume。

## 7. 如何通过 URL 隐藏左侧菜单
在 URL 查询参数中加入以下任意一个即可隐藏左侧菜单（适合嵌入模式）：
- `hideSidebar=1|true|yes|on`
- `embed=1|true|yes|on`
- `sidebar=0|false|no|off|hide`

示例：
```text
/settings?hideSidebar=1
/realtime?embed=1
/logs?sidebar=0
```
