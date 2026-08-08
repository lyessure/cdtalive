import os
from pathlib import Path

import yaml


DEFAULTS = {
    "region_id": "cn-hongkong",
    "traffic_threshold_gb": 190.0,
    "balance_threshold_cny": 1.0,
    "daily_stop_windows": [],
    "daily_stop_weekdays": [1, 2, 3, 4, 5, 6, 7],
    "daily_start_schedules": [],
    "power_mode": "auto",
    "run_interval_seconds": 300,
}
ENV_KEYS = {
    "access_key_id": "CDT_ACCESS_KEY_ID",
    "access_key_secret": "CDT_ACCESS_KEY_SECRET",
    "ecs_instance_id": "CDT_ECS_INSTANCE_ID",
    "region_id": "CDT_REGION_ID",
    "traffic_threshold_gb": "CDT_TRAFFIC_THRESHOLD_GB",
    "balance_threshold_cny": "CDT_BALANCE_THRESHOLD_CNY",
    "daily_stop_windows": "CDT_DAILY_STOP_WINDOWS",
    "daily_stop_weekdays": "CDT_DAILY_STOP_WEEKDAYS",
    "daily_start_schedules": "CDT_DAILY_START_SCHEDULES",
    "power_mode": "CDT_POWER_MODE",
    "run_interval_seconds": "CDT_RUN_INTERVAL_SECONDS",
}


def config_path() -> Path:
    return Path(os.getenv("CDT_CONFIG_FILE", "/data/cdtalive.yaml"))


def data_dir() -> Path:
    return Path(os.getenv("CDT_DATA_DIR", "/data"))


def load_config() -> dict:
    path = config_path()
    file_config = {}
    if path.exists():
        with path.open(encoding="utf-8") as fh:
            file_config = yaml.safe_load(fh) or {}
        if not isinstance(file_config, dict):
            raise ValueError("配置文件顶层必须是 YAML 对象")
    config = {**DEFAULTS, **file_config}
    for key, env_key in ENV_KEYS.items():
        value = os.getenv(env_key)
        if value not in (None, ""):
            config[key] = value
    windows = config.get("daily_stop_windows", [])
    if isinstance(windows, str):
        config["daily_stop_windows"] = [item.strip() for item in windows.split(",") if item.strip()]
    weekdays = config.get("daily_stop_weekdays", [1, 2, 3, 4, 5, 6, 7])
    if isinstance(weekdays, str):
        weekdays = [item.strip() for item in weekdays.split(",") if item.strip()]
    config["daily_stop_weekdays"] = validate_stop_weekdays(weekdays)
    legacy_stop_windows_override = bool(os.getenv("CDT_DAILY_STOP_WINDOWS"))
    start_schedules_override = bool(os.getenv("CDT_DAILY_START_SCHEDULES"))
    migrated_start_schedules = (
        not start_schedules_override
        and (legacy_stop_windows_override or "daily_start_schedules" not in file_config)
    )
    if migrated_start_schedules:
        config["daily_start_schedules"] = migrate_stop_schedules(
            config.get("daily_stop_windows", []), config.get("daily_stop_weekdays", [1, 2, 3, 4, 5, 6, 7])
        )
    elif isinstance(config.get("daily_start_schedules"), str):
        config["daily_start_schedules"] = yaml.safe_load(config["daily_start_schedules"]) or []
    for key in ("traffic_threshold_gb", "balance_threshold_cny", "run_interval_seconds"):
        config[key] = float(config[key])
    config["run_interval_seconds"] = max(int(config["run_interval_seconds"]), 60)
    config["daily_start_schedules"] = validate_start_schedules(config.get("daily_start_schedules", []))
    if migrated_start_schedules and not legacy_stop_windows_override:
        persist_migrated_start_schedules(config["daily_start_schedules"])
    config["power_mode"] = validate_power_mode(config.get("power_mode", "auto"))
    return config


def validate_config(config: dict) -> None:
    missing = [key for key in ("access_key_id", "access_key_secret", "ecs_instance_id") if not config.get(key)]
    if missing:
        raise ValueError("缺少必填配置: " + ", ".join(missing))


def validate_power_mode(mode: str) -> str:
    if not isinstance(mode, str) or mode not in {"on", "auto", "off"}:
        raise ValueError("无效开关机模式；仅支持 on、auto、off")
    return mode


def validate_stop_windows(windows: list[str]) -> list[str]:
    normalized = []
    for raw in windows:
        try:
            start, end = (part.strip() for part in raw.split("-", 1))
            for value in (start, end):
                hour, minute = (int(part) for part in value.split(":"))
                if not (0 <= hour <= 23 and 0 <= minute <= 59):
                    raise ValueError
            if start == end:
                raise ValueError
        except (ValueError, AttributeError):
            raise ValueError(f"无效停机时间段：{raw}；格式应为 HH:MM-HH:MM") from None
        normalized.append(f"{start}-{end}")
    return normalized


def validate_stop_weekdays(weekdays: list[int]) -> list[int]:
    if not isinstance(weekdays, list):
        raise ValueError("daily_stop_weekdays 必须是数字列表")
    normalized = []
    for weekday in weekdays:
        try:
            value = int(weekday)
        except (TypeError, ValueError):
            raise ValueError("无效停机星期；取值范围为 1-7（周一至周日）") from None
        if value < 1 or value > 7 or isinstance(weekday, bool):
            raise ValueError("无效停机星期；取值范围为 1-7（周一至周日）")
        if value not in normalized:
            normalized.append(value)
    return sorted(normalized)


def validate_start_windows(windows: list[str]) -> list[str]:
    if not isinstance(windows, list):
        raise ValueError("开机时间段必须是字符串列表")
    normalized = []
    for raw in windows:
        try:
            start, end = (part.strip() for part in raw.split("-", 1))
            start_hour, start_minute = (int(part) for part in start.split(":"))
            end_hour, end_minute = (int(part) for part in end.split(":"))
            if not (0 <= start_hour <= 23 and 0 <= start_minute <= 59):
                raise ValueError
            if end == "24:00":
                end_minutes = 1440
            else:
                if not (0 <= end_hour <= 23 and 0 <= end_minute <= 59):
                    raise ValueError
                end_minutes = end_hour * 60 + end_minute
            start_minutes = start_hour * 60 + start_minute
            if start_minutes == end_minutes:
                raise ValueError
        except (ValueError, AttributeError):
            raise ValueError(f"无效开机时间段：{raw}；格式应为 HH:MM-HH:MM（结束时间允许为 24:00）") from None
        normalized.append(f"{start_hour:02d}:{start_minute:02d}-{end_hour:02d}:{end_minute:02d}")
    return normalized


def validate_start_schedules(schedules: list[dict]) -> list[dict]:
    if not isinstance(schedules, list):
        raise ValueError("daily_start_schedules 必须是列表")
    normalized = []
    for schedule in schedules:
        if not isinstance(schedule, dict):
            raise ValueError("开机规则必须是对象")
        weekdays = validate_stop_weekdays(schedule.get("weekdays", []))
        windows = validate_start_windows(schedule.get("windows", []))
        if weekdays and windows:
            normalized.append({"weekdays": weekdays, "windows": windows})
    return normalized


def migrate_stop_schedules(windows, weekdays) -> list[dict]:
    selected = set(weekdays or [])
    parsed = []
    for window in windows or []:
        try:
            start_text, end_text = window.split("-", 1)
            start_hour, start_minute = (int(part) for part in start_text.strip().split(":"))
            end_hour, end_minute = (int(part) for part in end_text.strip().split(":"))
            parsed.append((start_hour * 60 + start_minute, end_hour * 60 + end_minute))
        except (ValueError, AttributeError):
            continue

    grouped = {}
    for weekday in range(1, 8):
        previous = 7 if weekday == 1 else weekday - 1
        stops = []
        for start, end in parsed:
            if start < end and weekday in selected:
                stops.append((start, end))
            elif start > end:
                if weekday in selected:
                    stops.append((start, 1440))
                if previous in selected:
                    stops.append((0, end))
        stops.sort()
        merged = []
        for start, end in stops:
            if start >= end:
                continue
            if merged and start <= merged[-1][1]:
                merged[-1] = (merged[-1][0], max(merged[-1][1], end))
            else:
                merged.append((start, end))
        starts = []
        cursor = 0
        for start, end in merged:
            if cursor < start:
                starts.append(format_schedule_window(cursor, start))
            cursor = max(cursor, end)
        if cursor < 1440:
            starts.append(format_schedule_window(cursor, 1440))
        key = tuple(starts)
        grouped.setdefault(key, []).append(weekday)
    return [{"weekdays": days, "windows": list(windows)} for windows, days in grouped.items() if windows]


def format_schedule_window(start: int, end: int) -> str:
    return f"{start // 60:02d}:{start % 60:02d}-{end // 60:02d}:{end % 60:02d}"


def persist_migrated_start_schedules(schedules: list[dict]) -> None:
    path = config_path()
    try:
        with path.open(encoding="utf-8") as fh:
            raw = yaml.safe_load(fh) or {}
    except FileNotFoundError:
        return
    if not isinstance(raw, dict):
        raise ValueError("配置文件顶层必须是 YAML 对象")
    raw["daily_start_schedules"] = schedules
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as fh:
        yaml.safe_dump(raw, fh, allow_unicode=True, sort_keys=False)
    os.chmod(path, 0o600)


def save_stop_windows(windows: list[str]) -> list[str]:
    windows = validate_stop_windows(windows)
    path = config_path()
    raw = {}
    if path.exists():
        with path.open(encoding="utf-8") as fh:
            raw = yaml.safe_load(fh) or {}
    if not isinstance(raw, dict):
        raise ValueError("配置文件顶层必须是 YAML 对象")
    raw["daily_stop_windows"] = windows
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as fh:
        yaml.safe_dump(raw, fh, allow_unicode=True, sort_keys=False)
    os.chmod(path, 0o600)
    return windows


def save_settings(schedules: list[dict], power_mode: str) -> tuple[list[dict], str]:
    schedules = validate_start_schedules(schedules)
    power_mode = validate_power_mode(power_mode)
    path = config_path()
    raw = {}
    if path.exists():
        with path.open(encoding="utf-8") as fh:
            raw = yaml.safe_load(fh) or {}
    if not isinstance(raw, dict):
        raise ValueError("配置文件顶层必须是 YAML 对象")
    raw["daily_start_schedules"] = schedules
    raw["power_mode"] = power_mode
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as fh:
        yaml.safe_dump(raw, fh, allow_unicode=True, sort_keys=False)
    os.chmod(path, 0o600)
    return schedules, power_mode


def init_config(config_data: dict) -> None:
    required_keys = (
        "access_key_id",
        "access_key_secret",
        "ecs_instance_id",
        "region_id",
        "traffic_threshold_gb",
        "balance_threshold_cny",
        "run_interval_seconds",
    )
    missing = [key for key in required_keys if config_data.get(key) in (None, "")]
    if missing:
        raise ValueError("所有配置项均为必填项，缺少: " + ", ".join(missing))

    region_id = config_data.get("region_id")

    try:
        traffic_threshold_gb = float(config_data.get("traffic_threshold_gb", 190.0))
    except (ValueError, TypeError):
        raise ValueError("流量阈值 (traffic_threshold_gb) 必须是数字")

    try:
        balance_threshold_cny = float(config_data.get("balance_threshold_cny", 1.0))
    except (ValueError, TypeError):
        raise ValueError("余额阈值 (balance_threshold_cny) 必须是数字")

    try:
        run_interval_seconds = int(config_data.get("run_interval_seconds", 300))
    except (ValueError, TypeError):
        raise ValueError("检查间隔 (run_interval_seconds) 必须是整数")
    if run_interval_seconds < 60:
        raise ValueError("检查间隔 (run_interval_seconds) 不能小于 60 秒")

    daily_stop_windows = config_data.get("daily_stop_windows", [])
    if not isinstance(daily_stop_windows, list):
        daily_stop_windows = []
    daily_stop_weekdays = config_data.get("daily_stop_weekdays", [1, 2, 3, 4, 5, 6, 7])
    if not isinstance(daily_stop_weekdays, list):
        daily_stop_weekdays = [1, 2, 3, 4, 5, 6, 7]
    daily_start_schedules = config_data.get("daily_start_schedules")
    if daily_start_schedules is None:
        daily_start_schedules = migrate_stop_schedules(daily_stop_windows, daily_stop_weekdays)

    raw = {
        "access_key_id": str(config_data["access_key_id"]).strip(),
        "access_key_secret": str(config_data["access_key_secret"]).strip(),
        "ecs_instance_id": str(config_data["ecs_instance_id"]).strip(),
        "region_id": str(region_id).strip(),
        "traffic_threshold_gb": traffic_threshold_gb,
        "balance_threshold_cny": balance_threshold_cny,
        "run_interval_seconds": run_interval_seconds,
        "daily_stop_windows": daily_stop_windows,
        "daily_stop_weekdays": validate_stop_weekdays(daily_stop_weekdays),
        "daily_start_schedules": validate_start_schedules(daily_start_schedules),
        "power_mode": "auto",
    }

    path = config_path()
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as fh:
        yaml.safe_dump(raw, fh, allow_unicode=True, sort_keys=False)
    os.chmod(path, 0o600)
