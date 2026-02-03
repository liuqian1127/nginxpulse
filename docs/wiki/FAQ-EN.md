# FAQ

## 1. Parsed timestamps are wrong
Timezone issue. The project uses system timezone.
- Docker: mount `/etc/localtime:/etc/localtime:ro`
- Bare metal: set system timezone and restart

## 2. API returns 500
- Check PostgreSQL DSN and connectivity.
- Check port usage (8088/8089).
- See `var/nginxpulse_data/nginxpulse.log`.

## 3. Data is empty or incomplete
- Parsing not finished yet.
- Time range or filters are too strict.
- Log path is wrong or not readable (see "Docker Deployment Permissions" in the Deployment guide).

## 4. Reparse happens after restart
- Make sure no stale process is still running.
- Check if logs are rotated/replaced.
- Ensure `websites[].name` has not changed.

## 5. Migration dialog keeps showing
- Triggered when `nginxpulse.db` exists and `pg_migration_done` is missing.
- After migration, the marker is created and dialog disappears.

## 6. Does dev data persist?
`dev_local.sh` uses docker volume `nginxpulse_pgdata` by default.
Remove it manually if you want a clean start.

## 7. Hide the left sidebar via URL
Add any of the following query params to hide the left sidebar (useful for embeds):
- `hideSidebar=1|true|yes|on`
- `embed=1|true|yes|on`
- `sidebar=0|false|no|off|hide`

Examples:
```text
/settings?hideSidebar=1
/realtime?embed=1
/logs?sidebar=0
```
