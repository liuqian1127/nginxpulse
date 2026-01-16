package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	envConfigJSON        = "CONFIG_JSON"
	envWebsites          = "WEBSITES"
	envLogDestination    = "LOG_DEST"
	envTaskInterval      = "TASK_INTERVAL"
	envLogRetentionDays  = "LOG_RETENTION_DAYS"
	envServerPort        = "SERVER_PORT"
	envPVStatusCodes     = "PV_STATUS_CODES"
	envPVExcludePatterns = "PV_EXCLUDE_PATTERNS"
	envPVExcludeIPs      = "PV_EXCLUDE_IPS"
	envDemoMode          = "DEMO_MODE"
)

var (
	defaultStatusCodeInclude = []int{200}
	defaultExcludePatterns   = []string{
		"favicon.ico$",
		"robots.txt$",
		"sitemap.xml$",
		`\.(?:js|css|jpg|jpeg|png|gif|svg|webp|woff|woff2|ttf|eot|ico)$`,
		"^/api/",
		"^/ajax/",
		"^/health$",
		"^/_(?:nuxt|next)/",
		"rss.xml$",
		"feed.xml$",
		"atom.xml$",
	}
	defaultSystem = SystemConfig{
		LogDestination:   "file",
		TaskInterval:     "1m",
		LogRetentionDays: 30,
		DemoMode:         false,
	}
	defaultServer = ServerConfig{
		Port: ":8089",
	}
)

func DefaultConfig() Config {
	return Config{
		System: defaultSystem,
		Server: defaultServer,
		PVFilter: PVFilterConfig{
			StatusCodeInclude: copyIntSlice(defaultStatusCodeInclude),
			ExcludePatterns:   copyStringSlice(defaultExcludePatterns),
		},
	}
}

func loadConfig() (*Config, error) {
	cfg := &Config{}
	loaded := false

	if raw, key := getEnvValue(envConfigJSON); raw != "" {
		if err := json.Unmarshal([]byte(raw), cfg); err != nil {
			return nil, fmt.Errorf("解析 %s 失败: %w", key, err)
		}
		loaded = true
	} else {
		bytes, err := os.ReadFile(ConfigFile)
		if err == nil {
			if err := json.Unmarshal(bytes, cfg); err != nil {
				return nil, err
			}
			loaded = true
		} else if !os.IsNotExist(err) {
			return nil, err
		} else if !HasEnvConfigSource() {
			return nil, err
		}
	}

	if err := applyEnvOverrides(cfg); err != nil {
		return nil, err
	}
	applyDefaults(cfg)

	if !loaded && len(cfg.Websites) == 0 {
		return nil, fmt.Errorf("未提供网站配置")
	}

	return cfg, nil
}

// HasEnvConfigSource reports if config can be loaded from env vars.
func HasEnvConfigSource() bool {
	return hasEnvValue(envConfigJSON) || hasEnvValue(envWebsites)
}

func applyEnvOverrides(cfg *Config) error {
	if raw, key := getEnvValue(envWebsites); raw != "" {
		websites := []WebsiteConfig{}
		if err := json.Unmarshal([]byte(raw), &websites); err != nil {
			return fmt.Errorf("解析 %s 失败: %w", key, err)
		}
		cfg.Websites = websites
	}

	if raw, _ := getEnvValue(envLogDestination); raw != "" {
		cfg.System.LogDestination = raw
	}

	if raw, _ := getEnvValue(envTaskInterval); raw != "" {
		cfg.System.TaskInterval = raw
	}

	if raw, key := getEnvValue(envLogRetentionDays); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("解析 %s 失败: %w", key, err)
		}
		if parsed <= 0 {
			return fmt.Errorf("%s 必须大于0", key)
		}
		cfg.System.LogRetentionDays = parsed
	}

	if raw, key := getEnvValue(envDemoMode); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("解析 %s 失败: %w", key, err)
		}
		cfg.System.DemoMode = parsed
	}

	if raw, _ := getEnvValue(envServerPort); raw != "" {
		if !strings.Contains(raw, ":") {
			raw = ":" + raw
		}
		cfg.Server.Port = raw
	}

	if raw, key := getEnvValue(envPVStatusCodes); raw != "" {
		values, err := parseIntSlice(raw)
		if err != nil {
			return fmt.Errorf("解析 %s 失败: %w", key, err)
		}
		cfg.PVFilter.StatusCodeInclude = values
	}

	if raw, key := getEnvValue(envPVExcludePatterns); raw != "" {
		values, err := parseStringSliceJSON(raw)
		if err != nil {
			return fmt.Errorf("解析 %s 失败: %w", key, err)
		}
		cfg.PVFilter.ExcludePatterns = values
	}

	if raw, key := getEnvValue(envPVExcludeIPs); raw != "" {
		values, err := parseStringSliceFlexible(raw)
		if err != nil {
			return fmt.Errorf("解析 %s 失败: %w", key, err)
		}
		cfg.PVFilter.ExcludeIPs = values
	}

	return nil
}

func applyDefaults(cfg *Config) {
	if cfg.System.LogDestination == "" {
		cfg.System.LogDestination = defaultSystem.LogDestination
	}
	if cfg.System.TaskInterval == "" {
		cfg.System.TaskInterval = defaultSystem.TaskInterval
	}
	if cfg.System.LogRetentionDays <= 0 {
		cfg.System.LogRetentionDays = defaultSystem.LogRetentionDays
	}
	if cfg.Server.Port == "" {
		cfg.Server.Port = defaultServer.Port
	}
	if len(cfg.PVFilter.StatusCodeInclude) == 0 {
		cfg.PVFilter.StatusCodeInclude = copyIntSlice(defaultStatusCodeInclude)
	}
	if len(cfg.PVFilter.ExcludePatterns) == 0 {
		cfg.PVFilter.ExcludePatterns = copyStringSlice(defaultExcludePatterns)
	}
}

func parseStringSliceJSON(value string) ([]string, error) {
	values := []string{}
	if err := json.Unmarshal([]byte(value), &values); err != nil {
		return nil, err
	}
	return values, nil
}

func parseStringSliceFlexible(value string) ([]string, error) {
	if strings.HasPrefix(strings.TrimSpace(value), "[") {
		return parseStringSliceJSON(value)
	}
	values := []string{}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		values = append(values, item)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("值为空")
	}
	return values, nil
}

func parseIntSlice(value string) ([]int, error) {
	if strings.HasPrefix(strings.TrimSpace(value), "[") {
		values := []int{}
		if err := json.Unmarshal([]byte(value), &values); err != nil {
			return nil, err
		}
		return values, nil
	}

	values := []int{}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parsed, err := strconv.Atoi(item)
		if err != nil {
			return nil, err
		}
		values = append(values, parsed)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("值为空")
	}
	return values, nil
}

func copyStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	copied := make([]string, len(values))
	copy(copied, values)
	return copied
}

func copyIntSlice(values []int) []int {
	if len(values) == 0 {
		return nil
	}
	copied := make([]int, len(values))
	copy(copied, values)
	return copied
}

func hasEnvValue(keys ...string) bool {
	_, key := getEnvValue(keys...)
	return key != ""
}

func getEnvValue(keys ...string) (string, string) {
	for _, key := range keys {
		value := strings.TrimSpace(os.Getenv(key))
		if value != "" {
			return value, key
		}
	}
	return "", ""
}
