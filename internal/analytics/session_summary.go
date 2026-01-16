package analytics

import (
	"fmt"

	"github.com/likaia/nginxpulse/internal/store"
	"github.com/likaia/nginxpulse/internal/timeutil"
)

type SessionSummary struct {
	SessionCount       int     `json:"sessionCount"`
	BounceCount        int     `json:"bounceCount"`
	BounceRate         float64 `json:"bounceRate"`
	AvgDurationSeconds int64   `json:"avgDurationSeconds"`
}

func (s SessionSummary) GetType() string {
	return "session_summary"
}

type SessionSummaryStatsManager struct {
	repo *store.Repository
}

func NewSessionSummaryStatsManager(userRepoPtr *store.Repository) *SessionSummaryStatsManager {
	return &SessionSummaryStatsManager{
		repo: userRepoPtr,
	}
}

func (m *SessionSummaryStatsManager) Query(query StatsQuery) (StatsResult, error) {
	result := SessionSummary{}

	timeRange, ok := query.ExtraParam["timeRange"].(string)
	if !ok || timeRange == "" {
		return result, fmt.Errorf("timeRange 参数缺失")
	}

	startTime, endTime, err := timeutil.TimePeriod(timeRange)
	if err != nil {
		return result, fmt.Errorf("解析时间范围失败: %v", err)
	}

	tableName := fmt.Sprintf("%s_nginx_logs", query.WebsiteID)
	rows, err := m.repo.GetDB().Query(
		fmt.Sprintf(`
        SELECT timestamp, ip_id, ua_id
        FROM "%s" INDEXED BY idx_%s_session_key
        WHERE pageview_flag = 1 AND timestamp >= ? AND timestamp < ?
        ORDER BY ip_id, ua_id, timestamp`,
			tableName, query.WebsiteID),
		startTime.Unix(), endTime.Unix(),
	)
	if err != nil {
		return result, fmt.Errorf("查询会话摘要失败: %v", err)
	}
	defer rows.Close()

	var (
		currentKey     string
		lastTimestamp  int64
		startTimestamp int64
		endTimestamp   int64
		pageCount      int
		initialized    bool
		totalDuration  int64
	)

	for rows.Next() {
		var (
			timestamp int64
			ipID      int64
			uaID      int64
		)
		if err := rows.Scan(&timestamp, &ipID, &uaID); err != nil {
			return result, fmt.Errorf("解析会话摘要失败: %v", err)
		}

		key := fmt.Sprintf("%d|%d", ipID, uaID)
		if !initialized || key != currentKey || timestamp-lastTimestamp > sessionGapSeconds {
			if initialized {
				finalizeSessionSummary(&result, startTimestamp, endTimestamp, pageCount, &totalDuration)
			}
			currentKey = key
			startTimestamp = timestamp
			endTimestamp = timestamp
			pageCount = 1
			initialized = true
		} else {
			endTimestamp = timestamp
			pageCount++
		}
		lastTimestamp = timestamp
	}

	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("遍历会话摘要失败: %v", err)
	}

	if initialized {
		finalizeSessionSummary(&result, startTimestamp, endTimestamp, pageCount, &totalDuration)
	}

	if result.SessionCount > 0 {
		result.BounceRate = float64(result.BounceCount) / float64(result.SessionCount)
		result.AvgDurationSeconds = totalDuration / int64(result.SessionCount)
	}

	return result, nil
}

func finalizeSessionSummary(result *SessionSummary, start, end int64, pageCount int, totalDuration *int64) {
	if result == nil || totalDuration == nil {
		return
	}
	if end < start {
		end = start
	}
	duration := end - start
	result.SessionCount++
	*totalDuration += duration
	if pageCount <= 1 {
		result.BounceCount++
	}
}
