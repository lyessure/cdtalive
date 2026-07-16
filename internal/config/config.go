package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	AccessKeyID        string   `yaml:"access_key_id" json:"access_key_id"`
	AccessKeySecret    string   `yaml:"access_key_secret" json:"access_key_secret"`
	ECSInstanceID      string   `yaml:"ecs_instance_id" json:"ecs_instance_id"`
	RegionID           string   `yaml:"region_id" json:"region_id"`
	TrafficThresholdGB float64  `yaml:"traffic_threshold_gb" json:"traffic_threshold_gb"`
	BalanceThreshold   float64  `yaml:"balance_threshold_cny" json:"balance_threshold_cny"`
	RunIntervalSeconds int      `yaml:"run_interval_seconds" json:"run_interval_seconds"`
	DailyStopWindows   []string `yaml:"daily_stop_windows" json:"daily_stop_windows"`
	PowerMode          string   `yaml:"power_mode" json:"power_mode"`
}

func defaults() Config {
	return Config{
		RegionID:           "cn-hongkong",
		TrafficThresholdGB: 190,
		BalanceThreshold:   1,
		RunIntervalSeconds: 300,
		DailyStopWindows:   []string{},
		PowerMode:          "auto",
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
	if value := os.Getenv("CDT_DAILY_STOP_WINDOWS"); value != "" {
		cfg.DailyStopWindows = splitWindows(value)
	}
	applyStringEnv(&cfg.PowerMode, "CDT_POWER_MODE")
	if cfg.RunIntervalSeconds < 60 {
		cfg.RunIntervalSeconds = 60
	}
	if cfg.DailyStopWindows == nil {
		cfg.DailyStopWindows = []string{}
	}
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
	cfg.PowerMode = "auto"
	return write(cfg)
}

func SaveSettings(windows []string, powerMode string) ([]string, string, error) {
	normalized, err := ValidateStopWindows(windows)
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
	cfg.DailyStopWindows = normalized
	cfg.PowerMode = powerMode
	if err := write(cfg); err != nil {
		return nil, "", err
	}
	return normalized, powerMode, nil
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
	return cfg, nil
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
