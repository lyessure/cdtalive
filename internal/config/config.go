package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	AccessKeyID         string          `yaml:"access_key_id" json:"access_key_id"`
	AccessKeySecret     string          `yaml:"access_key_secret" json:"access_key_secret"`
	ECSInstanceID       string          `yaml:"ecs_instance_id" json:"ecs_instance_id"`
	RegionID            string          `yaml:"region_id" json:"region_id"`
	TrafficThresholdGB  float64         `yaml:"traffic_threshold_gb" json:"traffic_threshold_gb"`
	BalanceThreshold    float64         `yaml:"balance_threshold_cny" json:"balance_threshold_cny"`
	RunIntervalSeconds  int             `yaml:"run_interval_seconds" json:"run_interval_seconds"`
	DailyStopWindows    []string        `yaml:"daily_stop_windows" json:"daily_stop_windows"`
	DailyStopWeekdays   []int           `yaml:"daily_stop_weekdays" json:"daily_stop_weekdays"`
	DailyStartSchedules []StartSchedule `yaml:"daily_start_schedules" json:"daily_start_schedules"`
	PowerMode           string          `yaml:"power_mode" json:"power_mode"`
}

type StartSchedule struct {
	Weekdays []int    `yaml:"weekdays" json:"weekdays"`
	Windows  []string `yaml:"windows" json:"windows"`
}

func defaults() Config {
	return Config{
		RegionID:            "cn-hongkong",
		TrafficThresholdGB:  190,
		BalanceThreshold:    1,
		RunIntervalSeconds:  300,
		DailyStopWindows:    []string{},
		DailyStopWeekdays:   defaultStopWeekdays(),
		DailyStartSchedules: nil,
		PowerMode:           "auto",
	}
}

func Path() string {
	if value := os.Getenv("CDT_CONFIG_FILE"); value != "" {
		return value
	}
	return filepath.Join("data", "cdtalive.yaml")
}

func Exists() bool {
	_, err := os.Stat(Path())
	return err == nil
}

func Load() (Config, error) {
	cfg := defaults()
	data, err := os.ReadFile(Path())
	if err != nil {
		return Config{}, err
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("读取配置文件失败: %w", err)
	}

	applyStringEnv(&cfg.AccessKeyID, "CDT_ACCESS_KEY_ID")
	applyStringEnv(&cfg.AccessKeySecret, "CDT_ACCESS_KEY_SECRET")
	applyStringEnv(&cfg.ECSInstanceID, "CDT_ECS_INSTANCE_ID")
	applyStringEnv(&cfg.RegionID, "CDT_REGION_ID")
	if err := applyFloatEnv(&cfg.TrafficThresholdGB, "CDT_TRAFFIC_THRESHOLD_GB"); err != nil {
		return Config{}, err
	}
	if err := applyFloatEnv(&cfg.BalanceThreshold, "CDT_BALANCE_THRESHOLD_CNY"); err != nil {
		return Config{}, err
	}
	if value := os.Getenv("CDT_RUN_INTERVAL_SECONDS"); value != "" {
		parsed, parseErr := strconv.ParseFloat(value, 64)
		if parseErr != nil {
			return Config{}, fmt.Errorf("CDT_RUN_INTERVAL_SECONDS 必须是数字")
		}
		cfg.RunIntervalSeconds = int(parsed)
	}
	legacyStopWindowsOverride := os.Getenv("CDT_DAILY_STOP_WINDOWS") != ""
	startSchedulesOverride := os.Getenv("CDT_DAILY_START_SCHEDULES") != ""
	if value := os.Getenv("CDT_DAILY_STOP_WINDOWS"); value != "" {
		cfg.DailyStopWindows = splitWindows(value)
	}
	if value := os.Getenv("CDT_DAILY_STOP_WEEKDAYS"); value != "" {
		weekdays, parseErr := splitWeekdays(value)
		if parseErr != nil {
			return Config{}, parseErr
		}
		cfg.DailyStopWeekdays = weekdays
	}
	if value := os.Getenv("CDT_DAILY_START_SCHEDULES"); value != "" {
		if err := yaml.Unmarshal([]byte(value), &cfg.DailyStartSchedules); err != nil {
			return Config{}, fmt.Errorf("CDT_DAILY_START_SCHEDULES 格式无效: %w", err)
		}
	}
	applyStringEnv(&cfg.PowerMode, "CDT_POWER_MODE")
	if cfg.RunIntervalSeconds < 60 {
		cfg.RunIntervalSeconds = 60
	}
	if cfg.DailyStopWindows == nil {
		cfg.DailyStopWindows = []string{}
	}
	if cfg.DailyStopWeekdays == nil {
		cfg.DailyStopWeekdays = defaultStopWeekdays()
	}
	weekdays, err := ValidateStopWeekdays(cfg.DailyStopWeekdays)
	if err != nil {
		return Config{}, err
	}
	cfg.DailyStopWeekdays = weekdays
	if legacyStopWindowsOverride && !startSchedulesOverride {
		cfg.DailyStartSchedules = migrateStopSchedules(cfg.DailyStopWindows, cfg.DailyStopWeekdays)
	} else if cfg.DailyStartSchedules == nil {
		cfg.DailyStartSchedules = migrateStopSchedules(cfg.DailyStopWindows, cfg.DailyStopWeekdays)
		if !startSchedulesOverride {
			if err := persistMigratedStartSchedules(cfg.DailyStartSchedules); err != nil {
				return Config{}, err
			}
		}
	}
	schedules, err := ValidateStartSchedules(cfg.DailyStartSchedules)
	if err != nil {
		return Config{}, err
	}
	cfg.DailyStartSchedules = schedules
	if err := ValidatePowerMode(cfg.PowerMode); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Validate(cfg Config) error {
	missing := make([]string, 0, 3)
	if cfg.AccessKeyID == "" {
		missing = append(missing, "access_key_id")
	}
	if cfg.AccessKeySecret == "" {
		missing = append(missing, "access_key_secret")
	}
	if cfg.ECSInstanceID == "" {
		missing = append(missing, "ecs_instance_id")
	}
	if len(missing) > 0 {
		return fmt.Errorf("缺少必填配置: %s", strings.Join(missing, ", "))
	}
	return nil
}

func Init(cfg Config) error {
	if Exists() {
		return errors.New("配置文件已存在，无法重复初始化。")
	}
	missing := make([]string, 0)
	if strings.TrimSpace(cfg.AccessKeyID) == "" {
		missing = append(missing, "access_key_id")
	}
	if strings.TrimSpace(cfg.AccessKeySecret) == "" {
		missing = append(missing, "access_key_secret")
	}
	if strings.TrimSpace(cfg.ECSInstanceID) == "" {
		missing = append(missing, "ecs_instance_id")
	}
	if strings.TrimSpace(cfg.RegionID) == "" {
		missing = append(missing, "region_id")
	}
	if len(missing) > 0 {
		return fmt.Errorf("所有配置项均为必填项，缺少: %s", strings.Join(missing, ", "))
	}
	if cfg.RunIntervalSeconds < 60 {
		return errors.New("检查间隔 (run_interval_seconds) 不能小于 60 秒")
	}
	cfg.AccessKeyID = strings.TrimSpace(cfg.AccessKeyID)
	cfg.AccessKeySecret = strings.TrimSpace(cfg.AccessKeySecret)
	cfg.ECSInstanceID = strings.TrimSpace(cfg.ECSInstanceID)
	cfg.RegionID = strings.TrimSpace(cfg.RegionID)
	if cfg.DailyStopWindows == nil {
		cfg.DailyStopWindows = []string{}
	}
	if cfg.DailyStopWeekdays == nil {
		cfg.DailyStopWeekdays = defaultStopWeekdays()
	}
	weekdays, err := ValidateStopWeekdays(cfg.DailyStopWeekdays)
	if err != nil {
		return err
	}
	cfg.DailyStopWeekdays = weekdays
	if cfg.DailyStartSchedules == nil {
		cfg.DailyStartSchedules = migrateStopSchedules(cfg.DailyStopWindows, cfg.DailyStopWeekdays)
	}
	schedules, err := ValidateStartSchedules(cfg.DailyStartSchedules)
	if err != nil {
		return err
	}
	cfg.DailyStartSchedules = schedules
	cfg.PowerMode = "auto"
	return write(cfg)
}

func SaveSettings(schedules []StartSchedule, powerMode string) ([]StartSchedule, string, error) {
	normalizedSchedules, err := ValidateStartSchedules(schedules)
	if err != nil {
		return nil, "", err
	}
	if err := ValidatePowerMode(powerMode); err != nil {
		return nil, "", err
	}
	cfg, err := loadFileOnly()
	if err != nil {
		return nil, "", err
	}
	cfg.DailyStartSchedules = normalizedSchedules
	cfg.PowerMode = powerMode
	if err := write(cfg); err != nil {
		return nil, "", err
	}
	return normalizedSchedules, powerMode, nil
}

func ValidatePowerMode(mode string) error {
	if mode != "on" && mode != "auto" && mode != "off" {
		return errors.New("无效开关机模式；仅支持 on、auto、off")
	}
	return nil
}

func ValidateStopWindows(windows []string) ([]string, error) {
	normalized := make([]string, 0, len(windows))
	for _, raw := range windows {
		parts := strings.SplitN(raw, "-", 2)
		if len(parts) != 2 {
			return nil, invalidWindow(raw)
		}
		start, okStart := normalizeClock(strings.TrimSpace(parts[0]))
		end, okEnd := normalizeClock(strings.TrimSpace(parts[1]))
		if !okStart || !okEnd || start == end {
			return nil, invalidWindow(raw)
		}
		normalized = append(normalized, start+"-"+end)
	}
	return normalized, nil
}

func ValidateStopWeekdays(weekdays []int) ([]int, error) {
	normalized := make([]int, 0, len(weekdays))
	seen := make(map[int]struct{}, len(weekdays))
	for _, weekday := range weekdays {
		if weekday < 1 || weekday > 7 {
			return nil, fmt.Errorf("无效停机星期：%d；取值范围为 1-7（周一至周日）", weekday)
		}
		if _, exists := seen[weekday]; exists {
			continue
		}
		seen[weekday] = struct{}{}
		normalized = append(normalized, weekday)
	}
	sort.Ints(normalized)
	return normalized, nil
}

func ValidateStartSchedules(schedules []StartSchedule) ([]StartSchedule, error) {
	normalized := make([]StartSchedule, 0, len(schedules))
	for _, schedule := range schedules {
		weekdays, err := ValidateStopWeekdays(schedule.Weekdays)
		if err != nil {
			return nil, err
		}
		windows, err := validateStartWindows(schedule.Windows)
		if err != nil {
			return nil, err
		}
		if len(weekdays) == 0 || len(windows) == 0 {
			continue
		}
		normalized = append(normalized, StartSchedule{Weekdays: weekdays, Windows: windows})
	}
	return normalized, nil
}

func validateStartWindows(windows []string) ([]string, error) {
	normalized := make([]string, 0, len(windows))
	for _, raw := range windows {
		parts := strings.SplitN(raw, "-", 2)
		if len(parts) != 2 {
			return nil, invalidStartWindow(raw)
		}
		start, okStart := normalizeScheduleClock(strings.TrimSpace(parts[0]), false)
		end, okEnd := normalizeScheduleClock(strings.TrimSpace(parts[1]), true)
		startMinutes := scheduleClockMinutes(start)
		endMinutes := scheduleClockMinutes(end)
		if !okStart || !okEnd || startMinutes == endMinutes {
			return nil, invalidStartWindow(raw)
		}
		normalized = append(normalized, start+"-"+end)
	}
	return normalized, nil
}

func normalizeScheduleClock(value string, allowEndOfDay bool) (string, bool) {
	if allowEndOfDay && value == "24:00" {
		return value, true
	}
	return normalizeClock(value)
}

func scheduleClockMinutes(value string) int {
	var hour, minute int
	fmt.Sscanf(value, "%d:%d", &hour, &minute)
	return hour*60 + minute
}

func invalidStartWindow(raw string) error {
	return fmt.Errorf("无效开机时间段：%s；格式应为 HH:MM-HH:MM（结束时间允许为 24:00）", raw)
}

type minuteInterval struct {
	start int
	end   int
}

func migrateStopSchedules(windows []string, weekdays []int) []StartSchedule {
	selected := make(map[int]bool)
	for _, weekday := range weekdays {
		if weekday >= 1 && weekday <= 7 {
			selected[weekday] = true
		}
	}
	stopIntervals := make([][2]int, 0, len(windows))
	for _, window := range windows {
		parts := strings.SplitN(window, "-", 2)
		if len(parts) != 2 {
			continue
		}
		start, okStart := normalizeClock(strings.TrimSpace(parts[0]))
		end, okEnd := normalizeClock(strings.TrimSpace(parts[1]))
		if !okStart || !okEnd || start == end {
			continue
		}
		stopIntervals = append(stopIntervals, [2]int{scheduleClockMinutes(start), scheduleClockMinutes(end)})
	}

	grouped := make(map[string]*StartSchedule)
	for weekday := 1; weekday <= 7; weekday++ {
		stops := make([]minuteInterval, 0, len(stopIntervals))
		previous := weekday - 1
		if previous == 0 {
			previous = 7
		}
		for _, interval := range stopIntervals {
			start, end := interval[0], interval[1]
			if start < end {
				if selected[weekday] {
					stops = append(stops, minuteInterval{start: start, end: end})
				}
			} else if start > end {
				if selected[weekday] {
					stops = append(stops, minuteInterval{start: start, end: 1440})
				}
				if selected[previous] {
					stops = append(stops, minuteInterval{start: 0, end: end})
				}
			}
		}
		windowsForDay := complementWindows(mergeIntervals(stops))
		key := strings.Join(windowsForDay, ",")
		if existing, ok := grouped[key]; ok {
			existing.Weekdays = append(existing.Weekdays, weekday)
		} else {
			grouped[key] = &StartSchedule{Weekdays: []int{weekday}, Windows: windowsForDay}
		}
	}

	result := make([]StartSchedule, 0, len(grouped))
	for _, schedule := range grouped {
		if len(schedule.Windows) == 0 {
			continue
		}
		result = append(result, *schedule)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Weekdays[0] < result[j].Weekdays[0] })
	return result
}

func mergeIntervals(intervals []minuteInterval) []minuteInterval {
	if len(intervals) < 2 {
		return intervals
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].start < intervals[j].start })
	merged := make([]minuteInterval, 0, len(intervals))
	for _, interval := range intervals {
		if interval.start >= interval.end {
			continue
		}
		if len(merged) == 0 || interval.start > merged[len(merged)-1].end {
			merged = append(merged, interval)
			continue
		}
		if interval.end > merged[len(merged)-1].end {
			merged[len(merged)-1].end = interval.end
		}
	}
	return merged
}

func complementWindows(stops []minuteInterval) []string {
	windows := make([]string, 0, len(stops)+1)
	start := 0
	for _, stop := range stops {
		if start < stop.start {
			windows = append(windows, formatScheduleWindow(start, stop.start))
		}
		if stop.end > start {
			start = stop.end
		}
	}
	if start < 1440 {
		windows = append(windows, formatScheduleWindow(start, 1440))
	}
	return windows
}

func formatScheduleWindow(start, end int) string {
	return fmt.Sprintf("%02d:%02d-%02d:%02d", start/60, start%60, end/60, end%60)
}

func Location() *time.Location {
	name := os.Getenv("CDT_TIMEZONE")
	if name == "" {
		name = "Asia/Shanghai"
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return time.Local
	}
	return location
}

func loadFileOnly() (Config, error) {
	cfg := defaults()
	data, err := os.ReadFile(Path())
	if err != nil {
		return Config{}, err
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("读取配置文件失败: %w", err)
	}
	if cfg.DailyStopWindows == nil {
		cfg.DailyStopWindows = []string{}
	}
	if cfg.DailyStopWeekdays == nil {
		cfg.DailyStopWeekdays = defaultStopWeekdays()
	}
	if cfg.DailyStartSchedules == nil {
		cfg.DailyStartSchedules = migrateStopSchedules(cfg.DailyStopWindows, cfg.DailyStopWeekdays)
	}
	schedules, err := ValidateStartSchedules(cfg.DailyStartSchedules)
	if err != nil {
		return Config{}, err
	}
	cfg.DailyStartSchedules = schedules
	return cfg, nil
}

func persistMigratedStartSchedules(schedules []StartSchedule) error {
	data, err := os.ReadFile(Path())
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("读取配置文件失败: %w", err)
	}
	if raw == nil {
		raw = make(map[string]any)
	}
	raw["daily_start_schedules"] = schedules
	migrated, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	if err := os.WriteFile(Path(), migrated, 0600); err != nil {
		return err
	}
	return os.Chmod(Path(), 0600)
}

func write(cfg Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	path := Path()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

func splitWindows(value string) []string {
	result := make([]string, 0)
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func splitWeekdays(value string) ([]int, error) {
	result := make([]int, 0)
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		weekday, err := strconv.Atoi(item)
		if err != nil {
			return nil, errors.New("CDT_DAILY_STOP_WEEKDAYS 必须是逗号分隔的 1-7 数字")
		}
		result = append(result, weekday)
	}
	return ValidateStopWeekdays(result)
}

func defaultStopWeekdays() []int {
	return []int{1, 2, 3, 4, 5, 6, 7}
}

func normalizeClock(value string) (string, bool) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return "", false
	}
	hour, errHour := strconv.Atoi(parts[0])
	minute, errMinute := strconv.Atoi(parts[1])
	if errHour != nil || errMinute != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return "", false
	}
	return fmt.Sprintf("%02d:%02d", hour, minute), true
}

func invalidWindow(raw string) error {
	return fmt.Errorf("无效停机时间段：%s；格式应为 HH:MM-HH:MM", raw)
}

func applyStringEnv(target *string, key string) {
	if value := os.Getenv(key); value != "" {
		*target = value
	}
}

func applyFloatEnv(target *float64, key string) error {
	if value := os.Getenv(key); value != "" {
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("%s 必须是数字", key)
		}
		*target = parsed
	}
	return nil
}
