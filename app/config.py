import os
from pathlib import Path

import yaml


DEFAULTS = {
    "region_id": "cn-hongkong",
    "traffic_threshold_gb": 190.0,
    "balance_threshold_cny": 1.0,
    "daily_stop_windows": [],
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
    for key in ("traffic_threshold_gb", "balance_threshold_cny", "run_interval_seconds"):
        config[key] = float(config[key])
    config["run_interval_seconds"] = max(int(config["run_interval_seconds"]), 60)
    windows = config.get("daily_stop_windows", [])
    if isinstance(windows, str):
        config["daily_stop_windows"] = [item.strip() for item in windows.split(",") if item.strip()]
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


def save_settings(windows: list[str], power_mode: str) -> tuple[list[str], str]:
    windows = validate_stop_windows(windows)
    power_mode = validate_power_mode(power_mode)
    path = config_path()
    raw = {}
    if path.exists():
        with path.open(encoding="utf-8") as fh:
            raw = yaml.safe_load(fh) or {}
    if not isinstance(raw, dict):
        raise ValueError("配置文件顶层必须是 YAML 对象")
    raw["daily_stop_windows"] = windows
    raw["power_mode"] = power_mode
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as fh:
        yaml.safe_dump(raw, fh, allow_unicode=True, sort_keys=False)
    os.chmod(path, 0o600)
    return windows, power_mode


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

    raw = {
        "access_key_id": str(config_data["access_key_id"]).strip(),
        "access_key_secret": str(config_data["access_key_secret"]).strip(),
        "ecs_instance_id": str(config_data["ecs_instance_id"]).strip(),
        "region_id": str(region_id).strip(),
        "traffic_threshold_gb": traffic_threshold_gb,
        "balance_threshold_cny": balance_threshold_cny,
        "run_interval_seconds": run_interval_seconds,
        "daily_stop_windows": daily_stop_windows,
        "power_mode": "auto",
    }

    path = config_path()
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as fh:
        yaml.safe_dump(raw, fh, allow_unicode=True, sort_keys=False)
    os.chmod(path, 0o600)
