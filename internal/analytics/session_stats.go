package analytics

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/likaia/nginxpulse/internal/store"
	"github.com/likaia/nginxpulse/internal/timeutil"
)

// SessionEntry 表示一条会话记录
type SessionEntry struct {
	IP               string `json:"ip"`
	DomesticLocation string `json:"domestic_location"`
	GlobalLocation   string `json:"global_location"`
	UserDevice       string `json:"user_device"`
	UserBrowser      string `json:"user_browser"`
	UserOS           string `json:"user_os"`
	StartTimestamp   int64  `json:"start_timestamp"`
	EndTimestamp     int64  `json:"end_timestamp"`
	StartTime        string `json:"start_time"`
	DurationSeconds  int64  `json:"duration_seconds"`
	PageCount        int    `json:"page_count"`
	EntryURL         string `json:"entry_url"`
	ExitURL          string `json:"exit_url"`
}

// SessionsStats 会话列表查询结果
type SessionsStats struct {
	Sessions   []SessionEntry `json:"sessions"`
	Pagination struct {
		Total    int `json:"total"`
		Page     int `json:"page"`
		PageSize int `json:"pageSize"`
		Pages    int `json:"pages"`
	} `json:"pagination"`
}

// GetType 实现 StatsResult 接口
func (s SessionsStats) GetType() string {
	return "session"
}

// SessionsStatsManager 实现会话查询功能
type SessionsStatsManager struct {
	repo *store.Repository
}

// NewSessionsStatsManager 创建会话查询管理器
func NewSessionsStatsManager(userRepoPtr *store.Repository) *SessionsStatsManager {
	return &SessionsStatsManager{
		repo: userRepoPtr,
	}
}

// Query 实现 StatsManager 接口
func (m *SessionsStatsManager) Query(query StatsQuery) (StatsResult, error) {
	result := SessionsStats{}

	page := 1
	pageSize := 100
	var timeRange string
	var timeStart int64
	var timeEnd int64
	var ipFilter string
	var deviceFilter string
	var browserFilter string
	var osFilter string

	if pageVal, ok := query.ExtraParam["page"].(int); ok && pageVal > 0 {
		page = pageVal
	}
	if pageSizeVal, ok := query.ExtraParam["pageSize"].(int); ok && pageSizeVal > 0 {
		pageSize = pageSizeVal
		if pageSize > 1000 {
			pageSize = 1000
		}
	}
	if timeRangeVal, ok := query.ExtraParam["timeRange"].(string); ok {
		timeRange = timeRangeVal
	}
	if timeStartVal, ok := query.ExtraParam["timeStart"].(string); ok && timeStartVal != "" {
		parsed, err := parseTimeFilter(timeStartVal)
		if err != nil {
			return result, fmt.Errorf("解析开始时间失败: %v", err)
		}
		timeStart = parsed
	}
	if timeEndVal, ok := query.ExtraParam["timeEnd"].(string); ok && timeEndVal != "" {
		parsed, err := parseTimeFilter(timeEndVal)
		if err != nil {
			return result, fmt.Errorf("解析结束时间失败: %v", err)
		}
		timeEnd = parsed
	}
	if ipFilterVal, ok := query.ExtraParam["ipFilter"].(string); ok {
		ipFilter = strings.TrimSpace(ipFilterVal)
	}
	if deviceFilterVal, ok := query.ExtraParam["deviceFilter"].(string); ok {
		deviceFilter = strings.TrimSpace(deviceFilterVal)
	}
	if browserFilterVal, ok := query.ExtraParam["browserFilter"].(string); ok {
		browserFilter = strings.TrimSpace(browserFilterVal)
	}
	if osFilterVal, ok := query.ExtraParam["osFilter"].(string); ok {
		osFilter = strings.TrimSpace(osFilterVal)
	}

	var queryBuilder strings.Builder
	queryBuilder.WriteString(fmt.Sprintf(`
        SELECT l.timestamp, l.ip_id, l.ua_id, ip.ip, ua.browser, ua.os, ua.device,
               u.url, loc.domestic, loc.global
        FROM "%s_nginx_logs" l INDEXED BY idx_%s_session_key
        JOIN "%s_dim_ip" ip ON ip.id = l.ip_id
        JOIN "%s_dim_ua" ua ON ua.id = l.ua_id
        JOIN "%s_dim_url" u ON u.id = l.url_id
        JOIN "%s_dim_location" loc ON loc.id = l.location_id`,
		query.WebsiteID, query.WebsiteID, query.WebsiteID, query.WebsiteID, query.WebsiteID, query.WebsiteID))

	conditions := make([]string, 0, 4)
	args := make([]interface{}, 0, 6)
	conditions = append(conditions, "pageview_flag = 1")

	if timeRange != "" {
		startTime, endTime, err := timeutil.TimePeriod(timeRange)
		if err != nil {
			return result, fmt.Errorf("解析时间范围失败: %v", err)
		}
		conditions = append(conditions, "timestamp >= ? AND timestamp < ?")
		args = append(args, startTime.Unix(), endTime.Unix())
	}
	if timeStart > 0 {
		conditions = append(conditions, "timestamp >= ?")
		args = append(args, timeStart)
	}
	if timeEnd > 0 {
		conditions = append(conditions, "timestamp <= ?")
		args = append(args, timeEnd)
	}
	if ipFilter != "" {
		conditions = append(conditions, "ip.ip LIKE ?")
		args = append(args, "%"+ipFilter+"%")
	}
	if deviceFilter != "" {
		conditions = append(conditions, "ua.device LIKE ?")
		args = append(args, "%"+deviceFilter+"%")
	}
	if browserFilter != "" {
		conditions = append(conditions, "ua.browser LIKE ?")
		args = append(args, "%"+browserFilter+"%")
	}
	if osFilter != "" {
		conditions = append(conditions, "ua.os LIKE ?")
		args = append(args, "%"+osFilter+"%")
	}

	if len(conditions) > 0 {
		queryBuilder.WriteString(" WHERE ")
		queryBuilder.WriteString(strings.Join(conditions, " AND "))
	}

	queryBuilder.WriteString(" ORDER BY l.ip_id, l.ua_id, l.timestamp")

	rows, err := m.repo.GetDB().Query(queryBuilder.String(), args...)
	if err != nil {
		return result, fmt.Errorf("查询会话日志失败: %v", err)
	}
	defer rows.Close()

	sessions := make([]SessionEntry, 0)
	var (
		currentKey    string
		lastTimestamp int64
		current       SessionEntry
		initialized   bool
	)

	for rows.Next() {
		var (
			timestamp int64
			ipID      int64
			uaID      int64
			ip        string
			browser   string
			os        string
			device    string
			url       string
			domestic  string
			global    string
		)

		if err := rows.Scan(&timestamp, &ipID, &uaID, &ip, &browser, &os, &device, &url, &domestic, &global); err != nil {
			return result, fmt.Errorf("解析会话日志失败: %v", err)
		}

		key := fmt.Sprintf("%d|%d", ipID, uaID)

		if !initialized || key != currentKey || timestamp-lastTimestamp > sessionGapSeconds {
			if initialized {
				finalizeSession(&current)
				sessions = append(sessions, current)
			}
			currentKey = key
			current = SessionEntry{
				IP:               ip,
				DomesticLocation: domestic,
				GlobalLocation:   global,
				UserDevice:       device,
				UserBrowser:      browser,
				UserOS:           os,
				StartTimestamp:   timestamp,
				EndTimestamp:     timestamp,
				EntryURL:         url,
				ExitURL:          url,
				PageCount:        1,
			}
			initialized = true
		} else {
			current.EndTimestamp = timestamp
			current.ExitURL = url
			current.PageCount++
		}

		lastTimestamp = timestamp
	}

	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("遍历会话日志失败: %v", err)
	}

	if initialized {
		finalizeSession(&current)
		sessions = append(sessions, current)
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartTimestamp > sessions[j].StartTimestamp
	})

	total := len(sessions)
	pages := 1
	if pageSize > 0 {
		pages = (total + pageSize - 1) / pageSize
	}
	if page < 1 {
		page = 1
	}
	start := (page - 1) * pageSize
	if start < 0 {
		start = 0
	}
	end := start + pageSize
	if start >= total {
		result.Sessions = []SessionEntry{}
	} else {
		if end > total {
			end = total
		}
		result.Sessions = sessions[start:end]
	}

	result.Pagination.Total = total
	result.Pagination.Page = page
	result.Pagination.PageSize = pageSize
	result.Pagination.Pages = pages

	return result, nil
}

func finalizeSession(session *SessionEntry) {
	if session == nil {
		return
	}
	if session.EndTimestamp < session.StartTimestamp {
		session.EndTimestamp = session.StartTimestamp
	}
	session.DurationSeconds = session.EndTimestamp - session.StartTimestamp
	session.StartTime = time.Unix(session.StartTimestamp, 0).Format("2006-01-02 15:04:05")
}
