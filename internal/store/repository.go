package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/likaia/nginxpulse/internal/config"
	"github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"
)

var (
	dataSourceName = filepath.Join(config.DataDir, "nginxpulse.db")
)

type NginxLogRecord struct {
	ID               int64     `json:"id"`
	IP               string    `json:"ip"`
	PageviewFlag     int       `json:"pageview_flag"`
	Timestamp        time.Time `json:"timestamp"`
	Method           string    `json:"method"`
	Url              string    `json:"url"`
	Status           int       `json:"status"`
	BytesSent        int       `json:"bytes_sent"`
	Referer          string    `json:"referer"`
	UserBrowser      string    `json:"user_browser"`
	UserOs           string    `json:"user_os"`
	UserDevice       string    `json:"user_device"`
	DomesticLocation string    `json:"domestic_location"`
	GlobalLocation   string    `json:"global_location"`
}

type Repository struct {
	db *sql.DB
}

func NewRepository() (*Repository, error) {
	// 打开数据库
	db, err := sql.Open("sqlite", dataSourceName)
	if err != nil {
		return nil, err
	}
	// 链接数据库
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	// 性能优化设置
	if _, err := db.Exec(`
        PRAGMA journal_mode=WAL;
        PRAGMA synchronous=NORMAL;
        PRAGMA cache_size=32768;
        PRAGMA temp_store=MEMORY;`); err != nil {
		db.Close()
		return nil, err
	}

	return &Repository{
		db: db,
	}, nil
}

// 初始化数据库
func (r *Repository) Init() error {
	return r.createTables()
}

// 关闭数据库连接
func (r *Repository) Close() error {
	logrus.Info("关闭数据库")
	if r.db != nil {
		return r.db.Close()
	}
	return nil
}

// 获取数据库连接
func (r *Repository) GetDB() *sql.DB {
	return r.db
}

// 为特定网站批量插入日志记录
func (r *Repository) BatchInsertLogsForWebsite(websiteID string, logs []NginxLogRecord) error {
	if len(logs) == 0 {
		return nil
	}

	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	// 准备批量插入语句
	logTable := fmt.Sprintf("%s_nginx_logs", websiteID)
	dims, err := prepareDimStatements(tx, websiteID)
	if err != nil {
		return err
	}
	defer dims.Close()
	aggs, err := prepareAggStatements(tx, websiteID)
	if err != nil {
		return err
	}
	defer aggs.Close()
	firstSeenStmt, err := prepareFirstSeenStatement(tx, websiteID)
	if err != nil {
		return err
	}
	defer firstSeenStmt.Close()
	sessions, err := prepareSessionStatements(tx, websiteID)
	if err != nil {
		return err
	}
	defer sessions.Close()

	stmtNginx, err := tx.Prepare(fmt.Sprintf(`
        INSERT INTO "%s" (
        ip_id, pageview_flag, timestamp, method, url_id, 
        status_code, bytes_sent, referer_id, ua_id, location_id)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `, logTable))
	if err != nil {
		return err
	}
	defer stmtNginx.Close()

	cache := newDimCaches()
	aggBatch := newAggBatch()
	sessionCache := make(map[string]sessionState)

	// 执行批量插入
	for _, log := range logs {
		ipID, err := getOrCreateDimID(
			cache.ip, dims.insertIP, dims.selectIP, log.IP, log.IP,
		)
		if err != nil {
			return err
		}

		urlID, err := getOrCreateDimID(
			cache.url, dims.insertURL, dims.selectURL, log.Url, log.Url,
		)
		if err != nil {
			return err
		}

		refererID, err := getOrCreateDimID(
			cache.referer, dims.insertReferer, dims.selectReferer, log.Referer, log.Referer,
		)
		if err != nil {
			return err
		}

		uaKey := uaCacheKey(log.UserBrowser, log.UserOs, log.UserDevice)
		uaID, err := getOrCreateDimID(
			cache.ua, dims.insertUA, dims.selectUA, uaKey,
			log.UserBrowser, log.UserOs, log.UserDevice,
		)
		if err != nil {
			return err
		}

		locationKey := locationCacheKey(log.DomesticLocation, log.GlobalLocation)
		locationID, err := getOrCreateDimID(
			cache.location, dims.insertLocation, dims.selectLocation, locationKey,
			log.DomesticLocation, log.GlobalLocation,
		)
		if err != nil {
			return err
		}

		_, err = stmtNginx.Exec(
			ipID, log.PageviewFlag, log.Timestamp.Unix(), log.Method, urlID,
			log.Status, log.BytesSent, refererID, uaID, locationID,
		)
		if err != nil {
			return err
		}

		if log.PageviewFlag == 1 {
			if _, err := firstSeenStmt.Exec(ipID, log.Timestamp.Unix()); err != nil {
				return err
			}
			if err := updateSessionFromLog(
				sessions,
				sessionCache,
				ipID,
				uaID,
				locationID,
				urlID,
				log.Timestamp.Unix(),
			); err != nil {
				return err
			}
		}

		aggBatch.add(log, ipID)
	}

	if err := applyAggUpdates(aggs, aggBatch); err != nil {
		return err
	}

	return tx.Commit()
}

// CleanOldLogs 清理保留天数之前的日志数据
func (r *Repository) CleanOldLogs() error {
	retentionDays := config.ReadConfig().System.LogRetentionDays
	if retentionDays <= 0 {
		retentionDays = 30
	}
	cutoffTime := time.Now().AddDate(0, 0, -retentionDays).Unix()
	cutoff := time.Unix(cutoffTime, 0)

	deletedCount := 0

	rows, err := r.db.Query(`
        SELECT name FROM sqlite_master 
        WHERE type='table' AND name LIKE '%_nginx_logs'
    `)
	if err != nil {
		return fmt.Errorf("查询表名失败: %v", err)
	}
	defer rows.Close()

	var tableNames []string
	for rows.Next() {
		var tableName string
		if err := rows.Scan(&tableName); err != nil {
			logrus.WithError(err).Error("扫描表名失败")
			continue
		}
		tableNames = append(tableNames, tableName)
	}

	for _, tableName := range tableNames {
		result, err := r.db.Exec(
			fmt.Sprintf(`DELETE FROM "%s" WHERE timestamp < ?`, tableName), cutoffTime,
		)
		if err != nil {
			logrus.WithError(err).Errorf("清理表 %s 的旧日志失败", tableName)
			continue
		}

		count, _ := result.RowsAffected()
		deletedCount += int(count)
	}

	if deletedCount > 0 {
		visited := make(map[string]struct{})
		for _, tableName := range tableNames {
			if !strings.HasSuffix(tableName, "_nginx_logs") {
				continue
			}
			websiteID := strings.TrimSuffix(tableName, "_nginx_logs")
			if websiteID == "" {
				continue
			}
			if _, ok := visited[websiteID]; ok {
				continue
			}
			visited[websiteID] = struct{}{}
			if err := r.cleanupOrphanDims(websiteID); err != nil {
				logrus.WithError(err).Warnf("清理网站 %s 的维表孤儿数据失败", websiteID)
			}
			if err := r.cleanupAggregates(websiteID, cutoff); err != nil {
				logrus.WithError(err).Warnf("清理网站 %s 的聚合数据失败", websiteID)
			}
			if err := r.cleanupSessions(websiteID, cutoff); err != nil {
				logrus.WithError(err).Warnf("清理网站 %s 的会话数据失败", websiteID)
			}
		}

		logrus.Infof("删除了 %d 条 %d 天前的日志记录", deletedCount, retentionDays)
		if _, err := r.db.Exec("VACUUM"); err != nil {
			logrus.WithError(err).Error("数据库压缩失败")
		}
	}

	return nil
}

// ClearLogsForWebsite 清空指定网站的日志数据
func (r *Repository) ClearLogsForWebsite(websiteID string) error {
	tableName := fmt.Sprintf("%s_nginx_logs", websiteID)
	if _, err := r.db.Exec(fmt.Sprintf(`DELETE FROM "%s"`, tableName)); err != nil {
		return fmt.Errorf("清空网站日志失败: %w", err)
	}
	if err := r.clearDimTablesForWebsite(websiteID); err != nil {
		return fmt.Errorf("清空网站维表失败: %w", err)
	}
	if err := r.clearFirstSeenForWebsite(websiteID); err != nil {
		return fmt.Errorf("清空网站首次访问数据失败: %w", err)
	}
	if err := r.clearAggregateTablesForWebsite(websiteID); err != nil {
		return fmt.Errorf("清空网站聚合表失败: %w", err)
	}
	if err := r.clearSessionTablesForWebsite(websiteID); err != nil {
		return fmt.Errorf("清空网站会话表失败: %w", err)
	}
	if err := r.clearSessionAggTablesForWebsite(websiteID); err != nil {
		return fmt.Errorf("清空网站会话聚合表失败: %w", err)
	}
	return nil
}

// ClearAllLogs 清空所有网站的日志数据
func (r *Repository) ClearAllLogs() error {
	for _, id := range config.GetAllWebsiteIDs() {
		if err := r.ClearLogsForWebsite(id); err != nil {
			return err
		}
	}
	if _, err := r.db.Exec("VACUUM"); err != nil {
		logrus.WithError(err).Warn("数据库压缩失败")
	}
	return nil
}

func (r *Repository) createTables() error {
	for _, id := range config.GetAllWebsiteIDs() {
		if err := r.ensureWebsiteSchema(id); err != nil {
			return err
		}
	}
	return nil
}

type sqlExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

type dimStatements struct {
	insertIP       *sql.Stmt
	selectIP       *sql.Stmt
	insertURL      *sql.Stmt
	selectURL      *sql.Stmt
	insertReferer  *sql.Stmt
	selectReferer  *sql.Stmt
	insertUA       *sql.Stmt
	selectUA       *sql.Stmt
	insertLocation *sql.Stmt
	selectLocation *sql.Stmt
}

type dimCaches struct {
	ip       map[string]int64
	url      map[string]int64
	referer  map[string]int64
	ua       map[string]int64
	location map[string]int64
}

type aggStatements struct {
	upsertHourly   *sql.Stmt
	upsertDaily    *sql.Stmt
	insertHourlyIP *sql.Stmt
	insertDailyIP  *sql.Stmt
}

type sessionStatements struct {
	selectState      *sql.Stmt
	upsertState      *sql.Stmt
	insertSession    *sql.Stmt
	updateSession    *sql.Stmt
	upsertDaily      *sql.Stmt
	upsertEntryDaily *sql.Stmt
}

type aggCounts struct {
	pv      int64
	traffic int64
	s2xx    int64
	s3xx    int64
	s4xx    int64
	s5xx    int64
	other   int64
}

type aggBatch struct {
	hourly    map[int64]*aggCounts
	daily     map[string]*aggCounts
	hourlyIPs map[int64]map[int64]struct{}
	dailyIPs  map[string]map[int64]struct{}
}

type sessionState struct {
	sessionID int64
	lastTs    int64
}

const sessionGapSeconds = int64(1800)

func newDimCaches() dimCaches {
	return dimCaches{
		ip:       make(map[string]int64),
		url:      make(map[string]int64),
		referer:  make(map[string]int64),
		ua:       make(map[string]int64),
		location: make(map[string]int64),
	}
}

func newAggBatch() *aggBatch {
	return &aggBatch{
		hourly:    make(map[int64]*aggCounts),
		daily:     make(map[string]*aggCounts),
		hourlyIPs: make(map[int64]map[int64]struct{}),
		dailyIPs:  make(map[string]map[int64]struct{}),
	}
}

func (d *dimStatements) Close() {
	closeStmt := func(stmt *sql.Stmt) {
		if stmt != nil {
			stmt.Close()
		}
	}
	closeStmt(d.insertIP)
	closeStmt(d.selectIP)
	closeStmt(d.insertURL)
	closeStmt(d.selectURL)
	closeStmt(d.insertReferer)
	closeStmt(d.selectReferer)
	closeStmt(d.insertUA)
	closeStmt(d.selectUA)
	closeStmt(d.insertLocation)
	closeStmt(d.selectLocation)
}

func (a *aggStatements) Close() {
	closeStmt := func(stmt *sql.Stmt) {
		if stmt != nil {
			stmt.Close()
		}
	}
	closeStmt(a.upsertHourly)
	closeStmt(a.upsertDaily)
	closeStmt(a.insertHourlyIP)
	closeStmt(a.insertDailyIP)
}

func (s *sessionStatements) Close() {
	closeStmt := func(stmt *sql.Stmt) {
		if stmt != nil {
			stmt.Close()
		}
	}
	closeStmt(s.selectState)
	closeStmt(s.upsertState)
	closeStmt(s.insertSession)
	closeStmt(s.updateSession)
	closeStmt(s.upsertDaily)
	closeStmt(s.upsertEntryDaily)
}

func prepareDimStatements(tx *sql.Tx, websiteID string) (*dimStatements, error) {
	ipTable := fmt.Sprintf("%s_dim_ip", websiteID)
	urlTable := fmt.Sprintf("%s_dim_url", websiteID)
	refererTable := fmt.Sprintf("%s_dim_referer", websiteID)
	uaTable := fmt.Sprintf("%s_dim_ua", websiteID)
	locationTable := fmt.Sprintf("%s_dim_location", websiteID)

	insertIP, err := tx.Prepare(fmt.Sprintf(`INSERT OR IGNORE INTO "%s" (ip) VALUES (?)`, ipTable))
	if err != nil {
		return nil, err
	}
	selectIP, err := tx.Prepare(fmt.Sprintf(`SELECT id FROM "%s" WHERE ip = ?`, ipTable))
	if err != nil {
		insertIP.Close()
		return nil, err
	}

	insertURL, err := tx.Prepare(fmt.Sprintf(`INSERT OR IGNORE INTO "%s" (url) VALUES (?)`, urlTable))
	if err != nil {
		selectIP.Close()
		insertIP.Close()
		return nil, err
	}
	selectURL, err := tx.Prepare(fmt.Sprintf(`SELECT id FROM "%s" WHERE url = ?`, urlTable))
	if err != nil {
		insertURL.Close()
		selectIP.Close()
		insertIP.Close()
		return nil, err
	}

	insertReferer, err := tx.Prepare(fmt.Sprintf(`INSERT OR IGNORE INTO "%s" (referer) VALUES (?)`, refererTable))
	if err != nil {
		selectURL.Close()
		insertURL.Close()
		selectIP.Close()
		insertIP.Close()
		return nil, err
	}
	selectReferer, err := tx.Prepare(fmt.Sprintf(`SELECT id FROM "%s" WHERE referer = ?`, refererTable))
	if err != nil {
		insertReferer.Close()
		selectURL.Close()
		insertURL.Close()
		selectIP.Close()
		insertIP.Close()
		return nil, err
	}

	insertUA, err := tx.Prepare(fmt.Sprintf(
		`INSERT OR IGNORE INTO "%s" (browser, os, device) VALUES (?, ?, ?)`, uaTable,
	))
	if err != nil {
		selectReferer.Close()
		insertReferer.Close()
		selectURL.Close()
		insertURL.Close()
		selectIP.Close()
		insertIP.Close()
		return nil, err
	}
	selectUA, err := tx.Prepare(fmt.Sprintf(
		`SELECT id FROM "%s" WHERE browser = ? AND os = ? AND device = ?`, uaTable,
	))
	if err != nil {
		insertUA.Close()
		selectReferer.Close()
		insertReferer.Close()
		selectURL.Close()
		insertURL.Close()
		selectIP.Close()
		insertIP.Close()
		return nil, err
	}

	insertLocation, err := tx.Prepare(fmt.Sprintf(
		`INSERT OR IGNORE INTO "%s" (domestic, global) VALUES (?, ?)`, locationTable,
	))
	if err != nil {
		selectUA.Close()
		insertUA.Close()
		selectReferer.Close()
		insertReferer.Close()
		selectURL.Close()
		insertURL.Close()
		selectIP.Close()
		insertIP.Close()
		return nil, err
	}
	selectLocation, err := tx.Prepare(fmt.Sprintf(
		`SELECT id FROM "%s" WHERE domestic = ? AND global = ?`, locationTable,
	))
	if err != nil {
		insertLocation.Close()
		selectUA.Close()
		insertUA.Close()
		selectReferer.Close()
		insertReferer.Close()
		selectURL.Close()
		insertURL.Close()
		selectIP.Close()
		insertIP.Close()
		return nil, err
	}

	return &dimStatements{
		insertIP:       insertIP,
		selectIP:       selectIP,
		insertURL:      insertURL,
		selectURL:      selectURL,
		insertReferer:  insertReferer,
		selectReferer:  selectReferer,
		insertUA:       insertUA,
		selectUA:       selectUA,
		insertLocation: insertLocation,
		selectLocation: selectLocation,
	}, nil
}

func prepareAggStatements(tx *sql.Tx, websiteID string) (*aggStatements, error) {
	hourlyTable := fmt.Sprintf("%s_agg_hourly", websiteID)
	dailyTable := fmt.Sprintf("%s_agg_daily", websiteID)
	hourlyIPTable := fmt.Sprintf("%s_agg_hourly_ip", websiteID)
	dailyIPTable := fmt.Sprintf("%s_agg_daily_ip", websiteID)

	upsertHourly, err := tx.Prepare(fmt.Sprintf(
		`INSERT INTO "%s" (bucket, pv, traffic, s2xx, s3xx, s4xx, s5xx, other)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?)
         ON CONFLICT(bucket) DO UPDATE SET
             pv = pv + excluded.pv,
             traffic = traffic + excluded.traffic,
             s2xx = s2xx + excluded.s2xx,
             s3xx = s3xx + excluded.s3xx,
             s4xx = s4xx + excluded.s4xx,
             s5xx = s5xx + excluded.s5xx,
             other = other + excluded.other`, hourlyTable,
	))
	if err != nil {
		return nil, err
	}

	upsertDaily, err := tx.Prepare(fmt.Sprintf(
		`INSERT INTO "%s" (day, pv, traffic, s2xx, s3xx, s4xx, s5xx, other)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?)
         ON CONFLICT(day) DO UPDATE SET
             pv = pv + excluded.pv,
             traffic = traffic + excluded.traffic,
             s2xx = s2xx + excluded.s2xx,
             s3xx = s3xx + excluded.s3xx,
             s4xx = s4xx + excluded.s4xx,
             s5xx = s5xx + excluded.s5xx,
             other = other + excluded.other`, dailyTable,
	))
	if err != nil {
		upsertHourly.Close()
		return nil, err
	}

	insertHourlyIP, err := tx.Prepare(fmt.Sprintf(
		`INSERT OR IGNORE INTO "%s" (bucket, ip_id) VALUES (?, ?)`, hourlyIPTable,
	))
	if err != nil {
		upsertDaily.Close()
		upsertHourly.Close()
		return nil, err
	}

	insertDailyIP, err := tx.Prepare(fmt.Sprintf(
		`INSERT OR IGNORE INTO "%s" (day, ip_id) VALUES (?, ?)`, dailyIPTable,
	))
	if err != nil {
		insertHourlyIP.Close()
		upsertDaily.Close()
		upsertHourly.Close()
		return nil, err
	}

	return &aggStatements{
		upsertHourly:   upsertHourly,
		upsertDaily:    upsertDaily,
		insertHourlyIP: insertHourlyIP,
		insertDailyIP:  insertDailyIP,
	}, nil
}

func prepareFirstSeenStatement(tx *sql.Tx, websiteID string) (*sql.Stmt, error) {
	table := fmt.Sprintf("%s_first_seen", websiteID)
	return tx.Prepare(fmt.Sprintf(
		`INSERT INTO "%s" (ip_id, first_ts)
         VALUES (?, ?)
         ON CONFLICT(ip_id) DO UPDATE SET
             first_ts = CASE
                 WHEN excluded.first_ts < first_ts THEN excluded.first_ts
                 ELSE first_ts
             END`, table,
	))
}

func prepareSessionStatements(tx *sql.Tx, websiteID string) (*sessionStatements, error) {
	stateTable := fmt.Sprintf("%s_session_state", websiteID)
	sessionTable := fmt.Sprintf("%s_sessions", websiteID)
	dailyTable := fmt.Sprintf("%s_agg_session_daily", websiteID)
	entryTable := fmt.Sprintf("%s_agg_entry_daily", websiteID)

	selectState, err := tx.Prepare(fmt.Sprintf(
		`SELECT session_id, last_ts FROM "%s" WHERE ip_id = ? AND ua_id = ?`, stateTable,
	))
	if err != nil {
		return nil, err
	}

	upsertState, err := tx.Prepare(fmt.Sprintf(
		`INSERT INTO "%s" (ip_id, ua_id, session_id, last_ts)
         VALUES (?, ?, ?, ?)
         ON CONFLICT(ip_id, ua_id) DO UPDATE SET
             session_id = excluded.session_id,
             last_ts = excluded.last_ts`, stateTable,
	))
	if err != nil {
		selectState.Close()
		return nil, err
	}

	insertSession, err := tx.Prepare(fmt.Sprintf(
		`INSERT INTO "%s" (ip_id, ua_id, location_id, start_ts, end_ts, entry_url_id, exit_url_id, page_count)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, sessionTable,
	))
	if err != nil {
		upsertState.Close()
		selectState.Close()
		return nil, err
	}

	updateSession, err := tx.Prepare(fmt.Sprintf(
		`UPDATE "%s" SET end_ts = ?, exit_url_id = ?, page_count = page_count + 1 WHERE id = ?`, sessionTable,
	))
	if err != nil {
		insertSession.Close()
		upsertState.Close()
		selectState.Close()
		return nil, err
	}

	upsertDaily, err := tx.Prepare(fmt.Sprintf(
		`INSERT INTO "%s" (day, sessions)
         VALUES (?, 1)
         ON CONFLICT(day) DO UPDATE SET
             sessions = sessions + 1`, dailyTable,
	))
	if err != nil {
		updateSession.Close()
		insertSession.Close()
		upsertState.Close()
		selectState.Close()
		return nil, err
	}

	upsertEntryDaily, err := tx.Prepare(fmt.Sprintf(
		`INSERT INTO "%s" (day, entry_url_id, count)
         VALUES (?, ?, 1)
         ON CONFLICT(day, entry_url_id) DO UPDATE SET
             count = count + 1`, entryTable,
	))
	if err != nil {
		upsertDaily.Close()
		updateSession.Close()
		insertSession.Close()
		upsertState.Close()
		selectState.Close()
		return nil, err
	}

	return &sessionStatements{
		selectState:      selectState,
		upsertState:      upsertState,
		insertSession:    insertSession,
		updateSession:    updateSession,
		upsertDaily:      upsertDaily,
		upsertEntryDaily: upsertEntryDaily,
	}, nil
}

func applyAggUpdates(aggs *aggStatements, batch *aggBatch) error {
	if aggs == nil || batch == nil {
		return nil
	}

	for bucket, counts := range batch.hourly {
		if counts == nil {
			continue
		}
		if _, err := aggs.upsertHourly.Exec(
			bucket,
			counts.pv,
			counts.traffic,
			counts.s2xx,
			counts.s3xx,
			counts.s4xx,
			counts.s5xx,
			counts.other,
		); err != nil {
			return err
		}
	}

	for day, counts := range batch.daily {
		if counts == nil {
			continue
		}
		if _, err := aggs.upsertDaily.Exec(
			day,
			counts.pv,
			counts.traffic,
			counts.s2xx,
			counts.s3xx,
			counts.s4xx,
			counts.s5xx,
			counts.other,
		); err != nil {
			return err
		}
	}

	for bucket, ips := range batch.hourlyIPs {
		for ipID := range ips {
			if _, err := aggs.insertHourlyIP.Exec(bucket, ipID); err != nil {
				return err
			}
		}
	}

	for day, ips := range batch.dailyIPs {
		for ipID := range ips {
			if _, err := aggs.insertDailyIP.Exec(day, ipID); err != nil {
				return err
			}
		}
	}

	return nil
}

func getOrCreateDimID(
	cache map[string]int64,
	insertStmt *sql.Stmt,
	selectStmt *sql.Stmt,
	cacheKey string,
	args ...any,
) (int64, error) {
	if id, ok := cache[cacheKey]; ok {
		return id, nil
	}
	if _, err := insertStmt.Exec(args...); err != nil {
		return 0, err
	}
	var id int64
	if err := selectStmt.QueryRow(args...).Scan(&id); err != nil {
		return 0, err
	}
	cache[cacheKey] = id
	return id, nil
}

func uaCacheKey(browser, osName, device string) string {
	return browser + "\x1f" + osName + "\x1f" + device
}

func locationCacheKey(domestic, global string) string {
	return domestic + "\x1f" + global
}

func (b *aggBatch) add(log NginxLogRecord, ipID int64) {
	if b == nil {
		return
	}
	hour := hourBucket(log.Timestamp)
	day := dayBucket(log.Timestamp)

	hourCounts := b.hourly[hour]
	if hourCounts == nil {
		hourCounts = &aggCounts{}
		b.hourly[hour] = hourCounts
	}
	dayCounts := b.daily[day]
	if dayCounts == nil {
		dayCounts = &aggCounts{}
		b.daily[day] = dayCounts
	}

	addCounts(hourCounts, log)
	addCounts(dayCounts, log)

	if log.PageviewFlag == 1 {
		if b.hourlyIPs[hour] == nil {
			b.hourlyIPs[hour] = make(map[int64]struct{})
		}
		b.hourlyIPs[hour][ipID] = struct{}{}
		if b.dailyIPs[day] == nil {
			b.dailyIPs[day] = make(map[int64]struct{})
		}
		b.dailyIPs[day][ipID] = struct{}{}
	}
}

func addCounts(counts *aggCounts, log NginxLogRecord) {
	if counts == nil {
		return
	}
	if log.PageviewFlag == 1 {
		counts.pv++
		counts.traffic += int64(log.BytesSent)
	}
	switch {
	case log.Status >= 200 && log.Status < 300:
		counts.s2xx++
	case log.Status >= 300 && log.Status < 400:
		counts.s3xx++
	case log.Status >= 400 && log.Status < 500:
		counts.s4xx++
	case log.Status >= 500 && log.Status < 600:
		counts.s5xx++
	default:
		counts.other++
	}
}

func updateSessionFromLog(
	stmts *sessionStatements,
	cache map[string]sessionState,
	ipID,
	uaID,
	locationID,
	urlID int64,
	timestamp int64,
) error {
	if stmts == nil {
		return nil
	}
	key := fmt.Sprintf("%d|%d", ipID, uaID)
	state, ok := cache[key]
	if !ok {
		var sessionID int64
		var lastTs int64
		if err := stmts.selectState.QueryRow(ipID, uaID).Scan(&sessionID, &lastTs); err == nil {
			state = sessionState{sessionID: sessionID, lastTs: lastTs}
		}
	}

	if state.sessionID != 0 && timestamp < state.lastTs {
		return nil
	}

	if state.sessionID == 0 || timestamp-state.lastTs > sessionGapSeconds {
		result, err := stmts.insertSession.Exec(
			ipID,
			uaID,
			locationID,
			timestamp,
			timestamp,
			urlID,
			urlID,
			1,
		)
		if err != nil {
			return err
		}
		sessionID, err := result.LastInsertId()
		if err != nil {
			return err
		}
		day := dayBucket(time.Unix(timestamp, 0))
		if stmts.upsertDaily != nil {
			if _, err := stmts.upsertDaily.Exec(day); err != nil {
				return err
			}
		}
		if stmts.upsertEntryDaily != nil {
			if _, err := stmts.upsertEntryDaily.Exec(day, urlID); err != nil {
				return err
			}
		}
		state = sessionState{sessionID: sessionID, lastTs: timestamp}
	} else {
		if _, err := stmts.updateSession.Exec(timestamp, urlID, state.sessionID); err != nil {
			return err
		}
		state.lastTs = timestamp
	}

	if _, err := stmts.upsertState.Exec(ipID, uaID, state.sessionID, state.lastTs); err != nil {
		return err
	}
	cache[key] = state
	return nil
}

func hourBucket(ts time.Time) int64 {
	local := ts.In(time.Local)
	start := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), 0, 0, 0, local.Location())
	return start.Unix()
}

func dayBucket(ts time.Time) string {
	return ts.In(time.Local).Format("2006-01-02")
}

func (r *Repository) cleanupOrphanDims(websiteID string) error {
	logTable := fmt.Sprintf("%s_nginx_logs", websiteID)
	hasIPID, err := r.tableHasColumn(logTable, "ip_id")
	if err != nil || !hasIPID {
		return err
	}

	type dimSpec struct {
		table  string
		column string
	}
	dims := []dimSpec{
		{table: fmt.Sprintf("%s_dim_ip", websiteID), column: "ip_id"},
		{table: fmt.Sprintf("%s_dim_url", websiteID), column: "url_id"},
		{table: fmt.Sprintf("%s_dim_referer", websiteID), column: "referer_id"},
		{table: fmt.Sprintf("%s_dim_ua", websiteID), column: "ua_id"},
		{table: fmt.Sprintf("%s_dim_location", websiteID), column: "location_id"},
	}

	for _, dim := range dims {
		exists, err := r.tableExists(dim.table)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if _, err := r.db.Exec(fmt.Sprintf(
			`DELETE FROM "%s" WHERE id NOT IN (SELECT %s FROM "%s")`,
			dim.table, dim.column, logTable,
		)); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) clearDimTablesForWebsite(websiteID string) error {
	dimTables := []string{
		fmt.Sprintf("%s_dim_ip", websiteID),
		fmt.Sprintf("%s_dim_url", websiteID),
		fmt.Sprintf("%s_dim_referer", websiteID),
		fmt.Sprintf("%s_dim_ua", websiteID),
		fmt.Sprintf("%s_dim_location", websiteID),
	}
	for _, table := range dimTables {
		exists, err := r.tableExists(table)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if _, err := r.db.Exec(fmt.Sprintf(`DELETE FROM "%s"`, table)); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) clearFirstSeenForWebsite(websiteID string) error {
	table := fmt.Sprintf("%s_first_seen", websiteID)
	exists, err := r.tableExists(table)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if _, err := r.db.Exec(fmt.Sprintf(`DELETE FROM "%s"`, table)); err != nil {
		return err
	}
	return nil
}

func (r *Repository) clearSessionTablesForWebsite(websiteID string) error {
	tables := []string{
		fmt.Sprintf("%s_sessions", websiteID),
		fmt.Sprintf("%s_session_state", websiteID),
	}
	for _, table := range tables {
		exists, err := r.tableExists(table)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if _, err := r.db.Exec(fmt.Sprintf(`DELETE FROM "%s"`, table)); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) clearSessionAggTablesForWebsite(websiteID string) error {
	tables := []string{
		fmt.Sprintf("%s_agg_session_daily", websiteID),
		fmt.Sprintf("%s_agg_entry_daily", websiteID),
	}
	for _, table := range tables {
		exists, err := r.tableExists(table)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if _, err := r.db.Exec(fmt.Sprintf(`DELETE FROM "%s"`, table)); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) ensureWebsiteSchema(websiteID string) error {
	logTable := fmt.Sprintf("%s_nginx_logs", websiteID)
	exists, err := r.tableExists(logTable)
	if err != nil {
		return err
	}

	if !exists {
		if err := createDimTables(r.db, websiteID); err != nil {
			return err
		}
		if err := createLogTable(r.db, logTable); err != nil {
			return err
		}
		if err := createLogIndexes(r.db, websiteID); err != nil {
			return err
		}
		if err := createAggTables(r.db, websiteID); err != nil {
			return err
		}
		if err := createFirstSeenTable(r.db, websiteID); err != nil {
			return err
		}
		if err := createSessionTables(r.db, websiteID); err != nil {
			return err
		}
		if err := createSessionAggTables(r.db, websiteID); err != nil {
			return err
		}
		if err := r.backfillAggregatesIfEmpty(websiteID); err != nil {
			return err
		}
		if err := r.backfillFirstSeenIfEmpty(websiteID); err != nil {
			return err
		}
		if err := r.backfillSessionsIfEmpty(websiteID); err != nil {
			return err
		}
		return r.backfillSessionAggregatesIfEmpty(websiteID)
	}

	hasIPID, err := r.tableHasColumn(logTable, "ip_id")
	if err != nil {
		return err
	}

	if !hasIPID {
		return r.migrateLegacyLogs(websiteID)
	}

	if err := createDimTables(r.db, websiteID); err != nil {
		return err
	}
	if err := createLogIndexes(r.db, websiteID); err != nil {
		return err
	}
	if err := createAggTables(r.db, websiteID); err != nil {
		return err
	}
	if err := createFirstSeenTable(r.db, websiteID); err != nil {
		return err
	}
	if err := createSessionTables(r.db, websiteID); err != nil {
		return err
	}
	if err := createSessionAggTables(r.db, websiteID); err != nil {
		return err
	}
	if err := r.backfillAggregatesIfEmpty(websiteID); err != nil {
		return err
	}
	if err := r.backfillFirstSeenIfEmpty(websiteID); err != nil {
		return err
	}
	if err := r.backfillSessionsIfEmpty(websiteID); err != nil {
		return err
	}
	return r.backfillSessionAggregatesIfEmpty(websiteID)
}

func (r *Repository) migrateLegacyLogs(websiteID string) error {
	logTable := fmt.Sprintf("%s_nginx_logs", websiteID)
	newLogTable := fmt.Sprintf("%s_nginx_logs_new", websiteID)

	logrus.WithField("website", websiteID).Info("检测到旧日志表结构，开始迁移")

	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if _, err = tx.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS "%s"`, newLogTable)); err != nil {
		return err
	}
	if err := createDimTables(tx, websiteID); err != nil {
		return err
	}
	if err := createLogTable(tx, newLogTable); err != nil {
		return err
	}
	if err := createAggTables(tx, websiteID); err != nil {
		return err
	}
	if err := createFirstSeenTable(tx, websiteID); err != nil {
		return err
	}
	if err := createSessionTables(tx, websiteID); err != nil {
		return err
	}
	if err := createSessionAggTables(tx, websiteID); err != nil {
		return err
	}

	if _, err = tx.Exec(fmt.Sprintf(
		`INSERT OR IGNORE INTO "%s_dim_ip"(ip) SELECT DISTINCT ip FROM "%s"`,
		websiteID, logTable,
	)); err != nil {
		return err
	}
	if _, err = tx.Exec(fmt.Sprintf(
		`INSERT OR IGNORE INTO "%s_dim_url"(url) SELECT DISTINCT url FROM "%s"`,
		websiteID, logTable,
	)); err != nil {
		return err
	}
	if _, err = tx.Exec(fmt.Sprintf(
		`INSERT OR IGNORE INTO "%s_dim_referer"(referer) SELECT DISTINCT referer FROM "%s"`,
		websiteID, logTable,
	)); err != nil {
		return err
	}
	if _, err = tx.Exec(fmt.Sprintf(
		`INSERT OR IGNORE INTO "%s_dim_ua"(browser, os, device)
         SELECT DISTINCT user_browser, user_os, user_device FROM "%s"`,
		websiteID, logTable,
	)); err != nil {
		return err
	}
	if _, err = tx.Exec(fmt.Sprintf(
		`INSERT OR IGNORE INTO "%s_dim_location"(domestic, global)
         SELECT DISTINCT domestic_location, global_location FROM "%s"`,
		websiteID, logTable,
	)); err != nil {
		return err
	}

	_, err = tx.Exec(fmt.Sprintf(
		`INSERT INTO "%s"(
            ip_id, pageview_flag, timestamp, method, url_id,
            status_code, bytes_sent, referer_id, ua_id, location_id
        )
        SELECT
            ip.id, l.pageview_flag, l.timestamp, l.method, url.id,
            l.status_code, l.bytes_sent, ref.id, ua.id, loc.id
        FROM "%s" l
        JOIN "%s_dim_ip" ip ON ip.ip = l.ip
        JOIN "%s_dim_url" url ON url.url = l.url
        JOIN "%s_dim_referer" ref ON ref.referer = l.referer
        JOIN "%s_dim_ua" ua
            ON ua.browser = l.user_browser AND ua.os = l.user_os AND ua.device = l.user_device
        JOIN "%s_dim_location" loc
            ON loc.domestic = l.domestic_location AND loc.global = l.global_location`,
		newLogTable, logTable,
		websiteID, websiteID, websiteID, websiteID, websiteID,
	))
	if err != nil {
		return err
	}

	if _, err = tx.Exec(fmt.Sprintf(`DROP TABLE "%s"`, logTable)); err != nil {
		return err
	}
	if _, err = tx.Exec(fmt.Sprintf(`ALTER TABLE "%s" RENAME TO "%s"`, newLogTable, logTable)); err != nil {
		return err
	}
	if err := createLogIndexes(tx, websiteID); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	if err := r.backfillAggregates(websiteID); err != nil {
		return err
	}
	if err := r.backfillFirstSeen(websiteID); err != nil {
		return err
	}
	if err := r.backfillSessions(websiteID); err != nil {
		return err
	}
	if err := r.backfillSessionAggregates(websiteID); err != nil {
		return err
	}

	logrus.WithField("website", websiteID).Info("旧日志表迁移完成")
	return nil
}

func (r *Repository) tableExists(tableName string) (bool, error) {
	row := r.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, tableName,
	)
	var name string
	if err := row.Scan(&name); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (r *Repository) tableHasRows(tableName string) (bool, error) {
	exists, err := r.tableExists(tableName)
	if err != nil || !exists {
		return false, err
	}
	row := r.db.QueryRow(fmt.Sprintf(`SELECT 1 FROM "%s" LIMIT 1`, tableName))
	var value int
	if err := row.Scan(&value); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (r *Repository) tableHasColumn(tableName, columnName string) (bool, error) {
	rows, err := r.db.Query(fmt.Sprintf(`PRAGMA table_info("%s")`, tableName))
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid       int
			name      string
			colType   string
			notnull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &colType, &notnull, &dfltValue, &pk); err != nil {
			return false, err
		}
		if name == columnName {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func createDimTables(execer sqlExecer, websiteID string) error {
	stmts := []string{
		fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS "%s_dim_ip" (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                ip TEXT NOT NULL UNIQUE
            )`, websiteID,
		),
		fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS "%s_dim_url" (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                url TEXT NOT NULL UNIQUE
            )`, websiteID,
		),
		fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS "%s_dim_referer" (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                referer TEXT NOT NULL UNIQUE
            )`, websiteID,
		),
		fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS "%s_dim_ua" (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                browser TEXT NOT NULL,
                os TEXT NOT NULL,
                device TEXT NOT NULL,
                UNIQUE(browser, os, device)
            )`, websiteID,
		),
		fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS "%s_dim_location" (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                domestic TEXT NOT NULL,
                global TEXT NOT NULL,
                UNIQUE(domestic, global)
            )`, websiteID,
		),
	}

	for _, stmt := range stmts {
		if _, err := execer.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func createLogTable(execer sqlExecer, tableName string) error {
	stmt := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS "%s" (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            ip_id INTEGER NOT NULL,
            pageview_flag INTEGER NOT NULL DEFAULT 0,
            timestamp INTEGER NOT NULL,
            method TEXT NOT NULL,
            url_id INTEGER NOT NULL,
            status_code INTEGER NOT NULL,
            bytes_sent INTEGER NOT NULL,
            referer_id INTEGER NOT NULL,
            ua_id INTEGER NOT NULL,
            location_id INTEGER NOT NULL
        )`, tableName,
	)
	_, err := execer.Exec(stmt)
	return err
}

func createLogIndexes(execer sqlExecer, websiteID string) error {
	tableName := fmt.Sprintf("%s_nginx_logs", websiteID)
	stmts := []string{
		fmt.Sprintf(
			`CREATE INDEX IF NOT EXISTS idx_%s_timestamp ON "%s"(timestamp)`,
			websiteID, tableName,
		),
		fmt.Sprintf(
			`CREATE INDEX IF NOT EXISTS idx_%s_pv_ts_ip ON "%s"(pageview_flag, timestamp, ip_id)`,
			websiteID, tableName,
		),
		fmt.Sprintf(
			`CREATE INDEX IF NOT EXISTS idx_%s_session_key ON "%s"(pageview_flag, ip_id, ua_id, timestamp)`,
			websiteID, tableName,
		),
	}
	for _, stmt := range stmts {
		if _, err := execer.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func createAggTables(execer sqlExecer, websiteID string) error {
	stmts := []string{
		fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS "%s_agg_hourly" (
                bucket INTEGER PRIMARY KEY,
                pv INTEGER NOT NULL DEFAULT 0,
                traffic INTEGER NOT NULL DEFAULT 0,
                s2xx INTEGER NOT NULL DEFAULT 0,
                s3xx INTEGER NOT NULL DEFAULT 0,
                s4xx INTEGER NOT NULL DEFAULT 0,
                s5xx INTEGER NOT NULL DEFAULT 0,
                other INTEGER NOT NULL DEFAULT 0
            )`, websiteID,
		),
		fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS "%s_agg_hourly_ip" (
                bucket INTEGER NOT NULL,
                ip_id INTEGER NOT NULL,
                PRIMARY KEY(bucket, ip_id)
            )`, websiteID,
		),
		fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS "%s_agg_daily" (
                day TEXT PRIMARY KEY,
                pv INTEGER NOT NULL DEFAULT 0,
                traffic INTEGER NOT NULL DEFAULT 0,
                s2xx INTEGER NOT NULL DEFAULT 0,
                s3xx INTEGER NOT NULL DEFAULT 0,
                s4xx INTEGER NOT NULL DEFAULT 0,
                s5xx INTEGER NOT NULL DEFAULT 0,
                other INTEGER NOT NULL DEFAULT 0
            )`, websiteID,
		),
		fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS "%s_agg_daily_ip" (
                day TEXT NOT NULL,
                ip_id INTEGER NOT NULL,
                PRIMARY KEY(day, ip_id)
            )`, websiteID,
		),
	}

	for _, stmt := range stmts {
		if _, err := execer.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func createFirstSeenTable(execer sqlExecer, websiteID string) error {
	stmt := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS "%s_first_seen" (
            ip_id INTEGER PRIMARY KEY,
            first_ts INTEGER NOT NULL
        )`, websiteID,
	)
	_, err := execer.Exec(stmt)
	return err
}

func createSessionTables(execer sqlExecer, websiteID string) error {
	stmts := []string{
		fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS "%s_sessions" (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                ip_id INTEGER NOT NULL,
                ua_id INTEGER NOT NULL,
                location_id INTEGER NOT NULL,
                start_ts INTEGER NOT NULL,
                end_ts INTEGER NOT NULL,
                entry_url_id INTEGER NOT NULL,
                exit_url_id INTEGER NOT NULL,
                page_count INTEGER NOT NULL DEFAULT 1
            )`, websiteID,
		),
		fmt.Sprintf(
			`CREATE INDEX IF NOT EXISTS idx_%s_sessions_start ON "%s_sessions"(start_ts)`,
			websiteID, websiteID,
		),
		fmt.Sprintf(
			`CREATE INDEX IF NOT EXISTS idx_%s_sessions_key ON "%s_sessions"(ip_id, ua_id, end_ts)`,
			websiteID, websiteID,
		),
		fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS "%s_session_state" (
                ip_id INTEGER NOT NULL,
                ua_id INTEGER NOT NULL,
                session_id INTEGER NOT NULL,
                last_ts INTEGER NOT NULL,
                PRIMARY KEY(ip_id, ua_id)
            )`, websiteID,
		),
	}
	for _, stmt := range stmts {
		if _, err := execer.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func createSessionAggTables(execer sqlExecer, websiteID string) error {
	stmts := []string{
		fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS "%s_agg_session_daily" (
                day TEXT PRIMARY KEY,
                sessions INTEGER NOT NULL DEFAULT 0
            )`, websiteID,
		),
		fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS "%s_agg_entry_daily" (
                day TEXT NOT NULL,
                entry_url_id INTEGER NOT NULL,
                count INTEGER NOT NULL DEFAULT 0,
                PRIMARY KEY(day, entry_url_id)
            )`, websiteID,
		),
	}
	for _, stmt := range stmts {
		if _, err := execer.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) backfillAggregatesIfEmpty(websiteID string) error {
	logTable := fmt.Sprintf("%s_nginx_logs", websiteID)
	aggHourly := fmt.Sprintf("%s_agg_hourly", websiteID)

	hasAgg, err := r.tableHasRows(aggHourly)
	if err != nil {
		return err
	}
	if hasAgg {
		return nil
	}

	hasLogs, err := r.tableHasRows(logTable)
	if err != nil || !hasLogs {
		return err
	}

	return r.backfillAggregates(websiteID)
}

func (r *Repository) backfillFirstSeenIfEmpty(websiteID string) error {
	table := fmt.Sprintf("%s_first_seen", websiteID)
	hasFirstSeen, err := r.tableHasRows(table)
	if err != nil {
		return err
	}
	if hasFirstSeen {
		return nil
	}

	logTable := fmt.Sprintf("%s_nginx_logs", websiteID)
	hasLogs, err := r.tableHasRows(logTable)
	if err != nil || !hasLogs {
		return err
	}

	return r.backfillFirstSeen(websiteID)
}

func (r *Repository) backfillSessionsIfEmpty(websiteID string) error {
	table := fmt.Sprintf("%s_sessions", websiteID)
	hasSessions, err := r.tableHasRows(table)
	if err != nil {
		return err
	}
	if hasSessions {
		return nil
	}

	logTable := fmt.Sprintf("%s_nginx_logs", websiteID)
	hasLogs, err := r.tableHasRows(logTable)
	if err != nil || !hasLogs {
		return err
	}

	return r.backfillSessions(websiteID)
}

func (r *Repository) backfillSessionAggregatesIfEmpty(websiteID string) error {
	dailyTable := fmt.Sprintf("%s_agg_session_daily", websiteID)
	entryTable := fmt.Sprintf("%s_agg_entry_daily", websiteID)

	hasDaily, err := r.tableHasRows(dailyTable)
	if err != nil {
		return err
	}
	hasEntry, err := r.tableHasRows(entryTable)
	if err != nil {
		return err
	}
	if hasDaily && hasEntry {
		return nil
	}

	sessionTable := fmt.Sprintf("%s_sessions", websiteID)
	hasSessions, err := r.tableHasRows(sessionTable)
	if err != nil || !hasSessions {
		return err
	}

	return r.backfillSessionAggregates(websiteID)
}

func (r *Repository) backfillAggregates(websiteID string) error {
	logTable := fmt.Sprintf("%s_nginx_logs", websiteID)
	aggHourly := fmt.Sprintf("%s_agg_hourly", websiteID)
	aggHourlyIP := fmt.Sprintf("%s_agg_hourly_ip", websiteID)
	aggDaily := fmt.Sprintf("%s_agg_daily", websiteID)
	aggDailyIP := fmt.Sprintf("%s_agg_daily_ip", websiteID)

	logrus.WithField("website", websiteID).Info("开始回填聚合数据")

	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if _, err = tx.Exec(fmt.Sprintf(`DELETE FROM "%s"`, aggHourly)); err != nil {
		return err
	}
	if _, err = tx.Exec(fmt.Sprintf(`DELETE FROM "%s"`, aggHourlyIP)); err != nil {
		return err
	}
	if _, err = tx.Exec(fmt.Sprintf(`DELETE FROM "%s"`, aggDaily)); err != nil {
		return err
	}
	if _, err = tx.Exec(fmt.Sprintf(`DELETE FROM "%s"`, aggDailyIP)); err != nil {
		return err
	}

	if _, err = tx.Exec(fmt.Sprintf(
		`INSERT INTO "%s" (bucket, pv, traffic, s2xx, s3xx, s4xx, s5xx, other)
         SELECT
             (timestamp / 3600) * 3600 AS bucket,
             SUM(CASE WHEN pageview_flag = 1 THEN 1 ELSE 0 END) AS pv,
             SUM(CASE WHEN pageview_flag = 1 THEN bytes_sent ELSE 0 END) AS traffic,
             SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END) AS s2xx,
             SUM(CASE WHEN status_code >= 300 AND status_code < 400 THEN 1 ELSE 0 END) AS s3xx,
             SUM(CASE WHEN status_code >= 400 AND status_code < 500 THEN 1 ELSE 0 END) AS s4xx,
             SUM(CASE WHEN status_code >= 500 AND status_code < 600 THEN 1 ELSE 0 END) AS s5xx,
             SUM(CASE WHEN status_code < 200 OR status_code >= 600 THEN 1 ELSE 0 END) AS other
         FROM "%s"
         GROUP BY bucket`, aggHourly, logTable,
	)); err != nil {
		return err
	}

	if _, err = tx.Exec(fmt.Sprintf(
		`INSERT OR IGNORE INTO "%s" (bucket, ip_id)
         SELECT
             (timestamp / 3600) * 3600 AS bucket,
             ip_id
         FROM "%s"
         WHERE pageview_flag = 1
         GROUP BY bucket, ip_id`, aggHourlyIP, logTable,
	)); err != nil {
		return err
	}

	if _, err = tx.Exec(fmt.Sprintf(
		`INSERT INTO "%s" (day, pv, traffic, s2xx, s3xx, s4xx, s5xx, other)
         SELECT
             strftime('%%Y-%%m-%%d', timestamp, 'unixepoch', 'localtime') AS day,
             SUM(CASE WHEN pageview_flag = 1 THEN 1 ELSE 0 END) AS pv,
             SUM(CASE WHEN pageview_flag = 1 THEN bytes_sent ELSE 0 END) AS traffic,
             SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END) AS s2xx,
             SUM(CASE WHEN status_code >= 300 AND status_code < 400 THEN 1 ELSE 0 END) AS s3xx,
             SUM(CASE WHEN status_code >= 400 AND status_code < 500 THEN 1 ELSE 0 END) AS s4xx,
             SUM(CASE WHEN status_code >= 500 AND status_code < 600 THEN 1 ELSE 0 END) AS s5xx,
             SUM(CASE WHEN status_code < 200 OR status_code >= 600 THEN 1 ELSE 0 END) AS other
         FROM "%s"
         GROUP BY day`, aggDaily, logTable,
	)); err != nil {
		return err
	}

	if _, err = tx.Exec(fmt.Sprintf(
		`INSERT OR IGNORE INTO "%s" (day, ip_id)
         SELECT
             strftime('%%Y-%%m-%%d', timestamp, 'unixepoch', 'localtime') AS day,
             ip_id
         FROM "%s"
         WHERE pageview_flag = 1
         GROUP BY day, ip_id`, aggDailyIP, logTable,
	)); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	logrus.WithField("website", websiteID).Info("聚合数据回填完成")
	return nil
}

func (r *Repository) backfillFirstSeen(websiteID string) error {
	logTable := fmt.Sprintf("%s_nginx_logs", websiteID)
	firstSeenTable := fmt.Sprintf("%s_first_seen", websiteID)

	logrus.WithField("website", websiteID).Info("开始回填首次访问数据")

	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if _, err = tx.Exec(fmt.Sprintf(`DELETE FROM "%s"`, firstSeenTable)); err != nil {
		return err
	}

	if _, err = tx.Exec(fmt.Sprintf(
		`INSERT INTO "%s" (ip_id, first_ts)
         SELECT ip_id, MIN(timestamp)
         FROM "%s"
         WHERE pageview_flag = 1
         GROUP BY ip_id`, firstSeenTable, logTable,
	)); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	logrus.WithField("website", websiteID).Info("首次访问数据回填完成")
	return nil
}

func (r *Repository) backfillSessions(websiteID string) error {
	logTable := fmt.Sprintf("%s_nginx_logs", websiteID)
	sessionTable := fmt.Sprintf("%s_sessions", websiteID)
	stateTable := fmt.Sprintf("%s_session_state", websiteID)

	logrus.WithField("website", websiteID).Info("开始回填会话数据")

	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if _, err = tx.Exec(fmt.Sprintf(`DELETE FROM "%s"`, sessionTable)); err != nil {
		return err
	}
	if _, err = tx.Exec(fmt.Sprintf(`DELETE FROM "%s"`, stateTable)); err != nil {
		return err
	}

	if _, err = tx.Exec(fmt.Sprintf(
		`WITH ordered AS (
            SELECT id, ip_id, ua_id, location_id, url_id, timestamp,
                   CASE
                       WHEN LAG(timestamp) OVER (
                           PARTITION BY ip_id, ua_id ORDER BY timestamp, id
                       ) IS NULL
                       OR timestamp - LAG(timestamp) OVER (
                           PARTITION BY ip_id, ua_id ORDER BY timestamp, id
                       ) > %d THEN 1
                       ELSE 0
                   END AS new_session
            FROM "%s"
            WHERE pageview_flag = 1
        ),
        sessions AS (
            SELECT *,
                   SUM(new_session) OVER (
                       PARTITION BY ip_id, ua_id ORDER BY timestamp, id
                       ROWS UNBOUNDED PRECEDING
                   ) AS session_no
            FROM ordered
        ),
        ranked AS (
            SELECT *,
                   ROW_NUMBER() OVER (
                       PARTITION BY ip_id, ua_id, session_no ORDER BY timestamp, id
                   ) AS rn_asc,
                   ROW_NUMBER() OVER (
                       PARTITION BY ip_id, ua_id, session_no ORDER BY timestamp DESC, id DESC
                   ) AS rn_desc
            FROM sessions
        )
        INSERT INTO "%s" (ip_id, ua_id, location_id, start_ts, end_ts, entry_url_id, exit_url_id, page_count)
        SELECT
            ip_id,
            ua_id,
            MAX(CASE WHEN rn_asc = 1 THEN location_id END) AS location_id,
            MIN(timestamp) AS start_ts,
            MAX(timestamp) AS end_ts,
            MAX(CASE WHEN rn_asc = 1 THEN url_id END) AS entry_url_id,
            MAX(CASE WHEN rn_desc = 1 THEN url_id END) AS exit_url_id,
            COUNT(*) AS page_count
        FROM ranked
        GROUP BY ip_id, ua_id, session_no`,
		sessionGapSeconds, logTable, sessionTable,
	)); err != nil {
		return err
	}

	if _, err = tx.Exec(fmt.Sprintf(
		`INSERT INTO "%s" (ip_id, ua_id, session_id, last_ts)
         SELECT ip_id, ua_id, id, end_ts
         FROM "%s"
         ORDER BY end_ts
         ON CONFLICT(ip_id, ua_id) DO UPDATE SET
             session_id = excluded.session_id,
             last_ts = excluded.last_ts`,
		stateTable, sessionTable,
	)); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	logrus.WithField("website", websiteID).Info("会话数据回填完成")
	return nil
}

func (r *Repository) backfillSessionAggregates(websiteID string) error {
	sessionTable := fmt.Sprintf("%s_sessions", websiteID)
	dailyTable := fmt.Sprintf("%s_agg_session_daily", websiteID)
	entryTable := fmt.Sprintf("%s_agg_entry_daily", websiteID)

	logrus.WithField("website", websiteID).Info("开始回填会话聚合数据")

	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if _, err = tx.Exec(fmt.Sprintf(`DELETE FROM "%s"`, dailyTable)); err != nil {
		return err
	}
	if _, err = tx.Exec(fmt.Sprintf(`DELETE FROM "%s"`, entryTable)); err != nil {
		return err
	}

	if _, err = tx.Exec(fmt.Sprintf(
		`INSERT INTO "%s" (day, sessions)
         SELECT
             strftime('%%Y-%%m-%%d', start_ts, 'unixepoch', 'localtime') AS day,
             COUNT(*)
         FROM "%s"
         GROUP BY day`, dailyTable, sessionTable,
	)); err != nil {
		return err
	}

	if _, err = tx.Exec(fmt.Sprintf(
		`INSERT INTO "%s" (day, entry_url_id, count)
         SELECT
             strftime('%%Y-%%m-%%d', start_ts, 'unixepoch', 'localtime') AS day,
             entry_url_id,
             COUNT(*)
         FROM "%s"
         GROUP BY day, entry_url_id`, entryTable, sessionTable,
	)); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	logrus.WithField("website", websiteID).Info("会话聚合数据回填完成")
	return nil
}

func (r *Repository) cleanupAggregates(websiteID string, cutoff time.Time) error {
	aggHourly := fmt.Sprintf("%s_agg_hourly", websiteID)
	aggHourlyIP := fmt.Sprintf("%s_agg_hourly_ip", websiteID)
	aggDaily := fmt.Sprintf("%s_agg_daily", websiteID)
	aggDailyIP := fmt.Sprintf("%s_agg_daily_ip", websiteID)

	hasAgg, err := r.tableExists(aggHourly)
	if err != nil || !hasAgg {
		return err
	}

	cutoffHour := hourBucket(cutoff)
	cutoffDay := dayBucket(cutoff)

	if _, err := r.db.Exec(fmt.Sprintf(`DELETE FROM "%s" WHERE bucket < ?`, aggHourly), cutoffHour); err != nil {
		return err
	}
	if _, err := r.db.Exec(fmt.Sprintf(`DELETE FROM "%s" WHERE bucket < ?`, aggHourlyIP), cutoffHour); err != nil {
		return err
	}
	if _, err := r.db.Exec(fmt.Sprintf(`DELETE FROM "%s" WHERE day < ?`, aggDaily), cutoffDay); err != nil {
		return err
	}
	if _, err := r.db.Exec(fmt.Sprintf(`DELETE FROM "%s" WHERE day < ?`, aggDailyIP), cutoffDay); err != nil {
		return err
	}

	if err := r.rebuildHourlyAggregate(websiteID, cutoffHour); err != nil {
		return err
	}
	if err := r.rebuildDailyAggregate(websiteID, cutoffDay); err != nil {
		return err
	}
	return r.rebuildFirstSeen(websiteID)
}

func (r *Repository) cleanupSessions(websiteID string, cutoff time.Time) error {
	sessionTable := fmt.Sprintf("%s_sessions", websiteID)
	stateTable := fmt.Sprintf("%s_session_state", websiteID)

	exists, err := r.tableExists(sessionTable)
	if err != nil || !exists {
		return err
	}

	if _, err := r.db.Exec(fmt.Sprintf(`DELETE FROM "%s" WHERE start_ts < ?`, sessionTable), cutoff.Unix()); err != nil {
		return err
	}

	stateExists, err := r.tableExists(stateTable)
	if err != nil || !stateExists {
		return err
	}

	if _, err := r.db.Exec(fmt.Sprintf(`DELETE FROM "%s"`, stateTable)); err != nil {
		return err
	}
	if _, err := r.db.Exec(fmt.Sprintf(
		`INSERT INTO "%s" (ip_id, ua_id, session_id, last_ts)
         SELECT ip_id, ua_id, id, end_ts
         FROM "%s"
         ORDER BY end_ts
         ON CONFLICT(ip_id, ua_id) DO UPDATE SET
             session_id = excluded.session_id,
             last_ts = excluded.last_ts`,
		stateTable, sessionTable,
	)); err != nil {
		return err
	}

	return r.cleanupSessionAggregates(websiteID, cutoff)
}

func (r *Repository) cleanupSessionAggregates(websiteID string, cutoff time.Time) error {
	dailyTable := fmt.Sprintf("%s_agg_session_daily", websiteID)
	entryTable := fmt.Sprintf("%s_agg_entry_daily", websiteID)

	hasDaily, err := r.tableExists(dailyTable)
	if err != nil || !hasDaily {
		return err
	}

	cutoffDay := dayBucket(cutoff)

	if _, err := r.db.Exec(fmt.Sprintf(`DELETE FROM "%s" WHERE day < ?`, dailyTable), cutoffDay); err != nil {
		return err
	}

	hasEntry, err := r.tableExists(entryTable)
	if err != nil {
		return err
	}
	if hasEntry {
		if _, err := r.db.Exec(fmt.Sprintf(`DELETE FROM "%s" WHERE day < ?`, entryTable), cutoffDay); err != nil {
			return err
		}
	}

	return r.rebuildSessionAggregatesForDay(websiteID, cutoffDay)
}

func (r *Repository) rebuildSessionAggregatesForDay(websiteID, day string) error {
	sessionTable := fmt.Sprintf("%s_sessions", websiteID)
	dailyTable := fmt.Sprintf("%s_agg_session_daily", websiteID)
	entryTable := fmt.Sprintf("%s_agg_entry_daily", websiteID)

	start, err := time.ParseInLocation("2006-01-02", day, time.Local)
	if err != nil {
		return err
	}
	end := start.Add(24 * time.Hour)

	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if _, err = tx.Exec(fmt.Sprintf(`DELETE FROM "%s" WHERE day = ?`, dailyTable), day); err != nil {
		return err
	}

	if _, err = tx.Exec(fmt.Sprintf(
		`INSERT INTO "%s" (day, sessions)
         SELECT ?, COUNT(*)
         FROM "%s"
         WHERE start_ts >= ? AND start_ts < ?
         HAVING COUNT(*) > 0`, dailyTable, sessionTable,
	), day, start.Unix(), end.Unix()); err != nil {
		return err
	}

	hasEntry, err := r.tableExists(entryTable)
	if err != nil {
		return err
	}
	if hasEntry {
		if _, err = tx.Exec(fmt.Sprintf(`DELETE FROM "%s" WHERE day = ?`, entryTable), day); err != nil {
			return err
		}
		if _, err = tx.Exec(fmt.Sprintf(
			`INSERT INTO "%s" (day, entry_url_id, count)
             SELECT ?, entry_url_id, COUNT(*)
             FROM "%s"
             WHERE start_ts >= ? AND start_ts < ?
             GROUP BY entry_url_id`, entryTable, sessionTable,
		), day, start.Unix(), end.Unix()); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (r *Repository) rebuildHourlyAggregate(websiteID string, bucket int64) error {
	logTable := fmt.Sprintf("%s_nginx_logs", websiteID)
	aggHourly := fmt.Sprintf("%s_agg_hourly", websiteID)
	aggHourlyIP := fmt.Sprintf("%s_agg_hourly_ip", websiteID)

	start := bucket
	end := bucket + 3600

	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if _, err = tx.Exec(fmt.Sprintf(`DELETE FROM "%s" WHERE bucket = ?`, aggHourly), bucket); err != nil {
		return err
	}
	if _, err = tx.Exec(fmt.Sprintf(`DELETE FROM "%s" WHERE bucket = ?`, aggHourlyIP), bucket); err != nil {
		return err
	}

	if _, err = tx.Exec(fmt.Sprintf(
		`INSERT INTO "%s" (bucket, pv, traffic, s2xx, s3xx, s4xx, s5xx, other)
         SELECT
             (timestamp / 3600) * 3600 AS bucket,
             SUM(CASE WHEN pageview_flag = 1 THEN 1 ELSE 0 END) AS pv,
             SUM(CASE WHEN pageview_flag = 1 THEN bytes_sent ELSE 0 END) AS traffic,
             SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END) AS s2xx,
             SUM(CASE WHEN status_code >= 300 AND status_code < 400 THEN 1 ELSE 0 END) AS s3xx,
             SUM(CASE WHEN status_code >= 400 AND status_code < 500 THEN 1 ELSE 0 END) AS s4xx,
             SUM(CASE WHEN status_code >= 500 AND status_code < 600 THEN 1 ELSE 0 END) AS s5xx,
             SUM(CASE WHEN status_code < 200 OR status_code >= 600 THEN 1 ELSE 0 END) AS other
         FROM "%s"
         WHERE timestamp >= ? AND timestamp < ?
         GROUP BY bucket`, aggHourly, logTable,
	), start, end); err != nil {
		return err
	}

	if _, err = tx.Exec(fmt.Sprintf(
		`INSERT OR IGNORE INTO "%s" (bucket, ip_id)
         SELECT
             (timestamp / 3600) * 3600 AS bucket,
             ip_id
         FROM "%s"
         WHERE pageview_flag = 1 AND timestamp >= ? AND timestamp < ?
         GROUP BY bucket, ip_id`, aggHourlyIP, logTable,
	), start, end); err != nil {
		return err
	}

	return tx.Commit()
}

func (r *Repository) rebuildDailyAggregate(websiteID string, day string) error {
	logTable := fmt.Sprintf("%s_nginx_logs", websiteID)
	aggDaily := fmt.Sprintf("%s_agg_daily", websiteID)
	aggDailyIP := fmt.Sprintf("%s_agg_daily_ip", websiteID)

	start, err := time.ParseInLocation("2006-01-02", day, time.Local)
	if err != nil {
		return err
	}
	end := start.Add(24 * time.Hour)

	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if _, err = tx.Exec(fmt.Sprintf(`DELETE FROM "%s" WHERE day = ?`, aggDaily), day); err != nil {
		return err
	}
	if _, err = tx.Exec(fmt.Sprintf(`DELETE FROM "%s" WHERE day = ?`, aggDailyIP), day); err != nil {
		return err
	}

	if _, err = tx.Exec(fmt.Sprintf(
		`INSERT INTO "%s" (day, pv, traffic, s2xx, s3xx, s4xx, s5xx, other)
         SELECT
             strftime('%%Y-%%m-%%d', timestamp, 'unixepoch', 'localtime') AS day,
             SUM(CASE WHEN pageview_flag = 1 THEN 1 ELSE 0 END) AS pv,
             SUM(CASE WHEN pageview_flag = 1 THEN bytes_sent ELSE 0 END) AS traffic,
             SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END) AS s2xx,
             SUM(CASE WHEN status_code >= 300 AND status_code < 400 THEN 1 ELSE 0 END) AS s3xx,
             SUM(CASE WHEN status_code >= 400 AND status_code < 500 THEN 1 ELSE 0 END) AS s4xx,
             SUM(CASE WHEN status_code >= 500 AND status_code < 600 THEN 1 ELSE 0 END) AS s5xx,
             SUM(CASE WHEN status_code < 200 OR status_code >= 600 THEN 1 ELSE 0 END) AS other
         FROM "%s"
         WHERE timestamp >= ? AND timestamp < ?
         GROUP BY day`, aggDaily, logTable,
	), start.Unix(), end.Unix()); err != nil {
		return err
	}

	if _, err = tx.Exec(fmt.Sprintf(
		`INSERT OR IGNORE INTO "%s" (day, ip_id)
         SELECT
             strftime('%%Y-%%m-%%d', timestamp, 'unixepoch', 'localtime') AS day,
             ip_id
         FROM "%s"
         WHERE pageview_flag = 1 AND timestamp >= ? AND timestamp < ?
         GROUP BY day, ip_id`, aggDailyIP, logTable,
	), start.Unix(), end.Unix()); err != nil {
		return err
	}

	return tx.Commit()
}

func (r *Repository) clearAggregateTablesForWebsite(websiteID string) error {
	aggTables := []string{
		fmt.Sprintf("%s_agg_hourly", websiteID),
		fmt.Sprintf("%s_agg_hourly_ip", websiteID),
		fmt.Sprintf("%s_agg_daily", websiteID),
		fmt.Sprintf("%s_agg_daily_ip", websiteID),
	}
	for _, table := range aggTables {
		exists, err := r.tableExists(table)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if _, err := r.db.Exec(fmt.Sprintf(`DELETE FROM "%s"`, table)); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) rebuildFirstSeen(websiteID string) error {
	table := fmt.Sprintf("%s_first_seen", websiteID)
	exists, err := r.tableExists(table)
	if err != nil || !exists {
		return err
	}
	return r.backfillFirstSeen(websiteID)
}
