import calendar
import json
import logging
import time
from datetime import datetime, timedelta
from pathlib import Path

from aliyunsdkcore.client import AcsClient
from aliyunsdkcore.request import CommonRequest
from aliyunsdkecs.request.v20140526 import DescribeInstancesRequest, StartInstancesRequest, StopInstancesRequest

from .config import data_dir, load_config, save_settings, validate_config

logger = logging.getLogger(__name__)


# 启停通常在几十秒内完成；过渡期间只查询 ECS 状态，避免高频请求余额和流量接口。
TRANSITION_POLL_INTERVAL_SECONDS = 15
TRANSITIONAL_ECS_STATES = {"Starting", "Stopping"}


class CdtService:
    def __init__(self):
        self.last_result: dict = {"status": "未执行"}
        self.next_run_at: int | None = None

    @property
    def status_file(self) -> Path:
        return data_dir() / "cdtalive_status.json"

    @property
    def balance_file(self) -> Path:
        return data_dir() / "cdtbal.json"

    @property
    def metrics_file(self) -> Path:
        return data_dir() / "latest_metrics.json"

    def _client(self, config):
        return AcsClient(config["access_key_id"], config["access_key_secret"], config["region_id"])

    @staticmethod
    def _request(domain, version, action):
        request = CommonRequest()
        request.set_domain(domain)
        request.set_version(version)
        request.set_action_name(action)
        request.set_method("POST")
        return request

    def _traffic_gb(self, client) -> float:
        response = client.do_action_with_exception(self._request("cdt.aliyuncs.com", "2021-08-13", "ListCdtInternetTraffic"))
        payload = json.loads(response.decode())
        return sum(item.get("Traffic", 0) for item in payload.get("TrafficDetails", [])) / 1024**3

    def _balance(self, client) -> float:
        response = client.do_action_with_exception(self._request("business.aliyuncs.com", "2017-12-14", "QueryAccountBalance"))
        payload = json.loads(response.decode())
        return float(payload.get("Data", {}).get("AvailableAmount", 0) or 0)

    def _instances(self, client):
        request = DescribeInstancesRequest.DescribeInstancesRequest()
        request.set_PageSize(100)
        response = client.do_action_with_exception(request)
        return json.loads(response.decode()).get("Instances", {}).get("Instance", [])

    def _instance_status(self, client, instance_id):
        instance = self._instance_info(client, instance_id)
        return instance.get("Status") if instance else None

    def _instance_info(self, client, instance_id):
        request = DescribeInstancesRequest.DescribeInstancesRequest()
        request.set_InstanceIds([instance_id])
        response = client.do_action_with_exception(request)
        instances = json.loads(response.decode()).get("Instances", {}).get("Instance", [])
        return instances[0] if instances else None

    @staticmethod
    def _cloud_start_timestamp(instance):
        if not instance:
            return None
        for field in ("StartTime", "LaunchTime", "StartedTime", "BootTime"):
            value = instance.get(field)
            if not value:
                continue
            try:
                return int(datetime.fromisoformat(str(value).replace("Z", "+00:00")).timestamp())
            except ValueError:
                continue
        return None

    def _record_instance_state(self, client, instance_id, fallback_status=None, reason=None):
        """Persist the latest ECS state and classify stop events.

        A scheduled stop is counted once when a daily stop window is entered.
        An unexpected stop is counted only when ECS changes from a running state
        to a stopped state outside of a stop action initiated by this service.
        """
        now = int(time.time())
        instance = self._instance_info(client, instance_id)
        status = (instance or {}).get("Status") or fallback_status
        if fallback_status == "Starting" and status in (None, "Stopped", "Stopping"):
            status = fallback_status
        if fallback_status == "Stopping" and status in (None, "Running", "Starting"):
            status = fallback_status
        stats_path = data_dir() / "ecs_status_history.json"
        stats = self._read_json(stats_path, {})
        if not isinstance(stats, dict):
            stats = {}

        cutoff = now - 30 * 86400
        for key in ("scheduled_downtime_timestamps", "unexpected_downtime_timestamps"):
            timestamps = stats.get(key, [])
            stats[key] = [timestamp for timestamp in timestamps if isinstance(timestamp, (int, float)) and timestamp >= cutoff]

        previous_status = stats.get("last_status")
        was_scheduled_stop_active = bool(stats.get("scheduled_stop_active"))
        is_scheduled_stop = reason == "scheduled_stop_window"
        service_initiated_stop = reason in {
            "scheduled_stop_window",
            "forced_off",
            "traffic_threshold_reached",
            "low_balance",
        }

        if is_scheduled_stop and not was_scheduled_stop_active:
            stats["scheduled_downtime_timestamps"].append(now)
            stats["last_scheduled_stop_timestamp"] = now
        stats["scheduled_stop_active"] = is_scheduled_stop

        if (
            status in ("Stopped", "Stopping")
            and previous_status in ("Running", "Starting")
            and not was_scheduled_stop_active
            and not service_initiated_stop
        ):
            stats["unexpected_downtime_timestamps"].append(now)
            stats["last_unexpected_stop_timestamp"] = now

        stats["last_status"] = status
        if status in ("Running", "Starting"):
            stats["last_startup_timestamp"] = self._cloud_start_timestamp(instance) or now
        data_dir().mkdir(parents=True, exist_ok=True)
        stats_path.write_text(json.dumps(stats, ensure_ascii=False), encoding="utf-8")
        return status

    def _start(self, client, instance_id):
        status = self._instance_status(client, instance_id)
        if status == "Running":
            return "already_running"
        request = StartInstancesRequest.StartInstancesRequest()
        request.set_InstanceIds([instance_id])
        request.set_accept_format("json")
        client.do_action_with_exception(request)
        return "start_requested"

    def _stop(self, client, instance_id):
        status = self._instance_status(client, instance_id)
        if status == "Stopped":
            return "already_stopped"
        request = StopInstancesRequest.StopInstancesRequest()
        request.set_InstanceIds([instance_id])
        request.set_ForceStop(False)
        request.set_StoppedMode("StopCharging")
        request.set_accept_format("json")
        client.do_action_with_exception(request)
        return "stop_requested"

    @staticmethod
    def _within_stop_window(windows):
        now = datetime.now()
        current = now.hour * 60 + now.minute
        for window in windows:
            start_text, end_text = window.split("-", 1)
            start_h, start_m = map(int, start_text.strip().split(":"))
            end_h, end_m = map(int, end_text.strip().split(":"))
            start, end = start_h * 60 + start_m, end_h * 60 + end_m
            if start == end:
                raise ValueError(f"无效定时停机时间段: {window}")
            if (start < end and start <= current < end) or (start > end and (current >= start or current < end)):
                return True
        return False

    @staticmethod
    def next_stop_window_boundary(windows, now=None):
        """Return the next daily stop-window boundary in local time."""
        now = now or datetime.now()
        candidates = []
        for window in windows:
            start_text, end_text = window.split("-", 1)
            for value in (start_text, end_text):
                hour, minute = map(int, value.strip().split(":"))
                candidate = now.replace(hour=hour, minute=minute, second=0, microsecond=0)
                if candidate <= now:
                    candidate += timedelta(days=1)
                candidates.append(candidate)
        return min(candidates) if candidates else None

    def _save_status(self, result):
        data_dir().mkdir(parents=True, exist_ok=True)
        self.status_file.write_text(json.dumps(result, ensure_ascii=False, indent=2), encoding="utf-8")

    def is_ecs_transitioning(self) -> bool:
        return self.last_result.get("ecs_status") in TRANSITIONAL_ECS_STATES

    def refresh_ecs_status(self) -> dict:
        """Refresh only the ECS state while an async start/stop is in progress."""
        config = load_config()
        validate_config(config)
        result = dict(self.last_result)
        client = self._client(config)
        result["ecs_checked_at"] = int(time.time())
        result["ecs_status"] = self._record_instance_state(
            client,
            config["ecs_instance_id"],
            result.get("ecs_status"),
            result.get("reason"),
        )
        self.last_result = result
        self._save_status(result)
        logger.info("ECS 状态快速检查完成：%s", result["ecs_status"])
        return result

    @staticmethod
    def _read_json(path, default):
        try:
            return json.loads(path.read_text(encoding="utf-8")) if path.exists() else default
        except (OSError, json.JSONDecodeError):
            return default

    @staticmethod
    def _spent_since(balance_history, since_timestamp):
        """Sum recorded balance decreases whose ending sample is in the period."""
        samples = []
        for item in balance_history:
            try:
                samples.append((float(item["timestamp"]), float(item["available_amount"])))
            except (KeyError, TypeError, ValueError):
                continue
        samples.sort(key=lambda sample: sample[0])

        spent = 0.0
        previous_amount = None
        for timestamp, amount in samples:
            if previous_amount is not None and timestamp >= since_timestamp:
                spent += max(previous_amount - amount, 0)
            previous_amount = amount
        return spent

    def dashboard(self) -> dict:
        """Return only the most recent values needed by the UI, never history."""
        result = self._read_json(self.status_file, self.last_result)
        metrics = self._read_json(self.metrics_file, {})
        if isinstance(metrics, dict):
            for key in ("traffic_gb", "traffic_threshold_gb", "daily_remaining_gb"):
                if key in metrics and key not in result:
                    result[key] = metrics[key]
        balance_history = self._read_json(self.balance_file, [])
        latest_balance = balance_history[-1] if isinstance(balance_history, list) and balance_history else None
        balance = None
        if latest_balance:
            baseline = min(balance_history, key=lambda item: abs(item.get("timestamp", 0) - (latest_balance["timestamp"] - 86400)))
            spent = max(0, baseline.get("available_amount", 0) - latest_balance.get("available_amount", 0))
            now = datetime.now()
            month_start = now.replace(day=1, hour=0, minute=0, second=0, microsecond=0)
            days = calendar.monthrange(now.year, now.month)[1]
            balance = {
                "available_cny": latest_balance.get("available_amount", 0),
                "spent_24h_cny": round(spent, 2),
                "spent_current_month_cny": round(
                    self._spent_since(balance_history, month_start.timestamp()),
                    2,
                ),
                "estimated_monthly_cny": round(spent * days, 2),
                "updated_at": latest_balance.get("timestamp"),
            }
        stats = self._read_json(data_dir() / "ecs_status_history.json", {})
        status = None
        if stats:
            now = int(time.time())
            startup = stats.get("last_startup_timestamp")
            current_state = result.get("ecs_status") or stats.get("last_status")
            status = {
                "state": current_state,
                "scheduled_stops_30d": sum(
                    1 for timestamp in stats.get("scheduled_downtime_timestamps", [])
                    if isinstance(timestamp, (int, float)) and timestamp >= now - 30 * 86400
                ),
                "unexpected_stops_30d": sum(
                    1 for timestamp in stats.get("unexpected_downtime_timestamps", [])
                    if isinstance(timestamp, (int, float)) and timestamp >= now - 30 * 86400
                ),
                "running_days": round((now - startup) / 86400, 2)
                if current_state in ("Running", "Starting") and startup and startup <= now
                else None,
                "scheduled_stop_active": (
                    result.get("reason") == "scheduled_stop_window"
                    if result.get("reason")
                    else bool(stats.get("scheduled_stop_active"))
                ),
            }
        return {
            "last_run": result,
            "balance": balance,
            "ecs": status,
            "run_interval_seconds": load_config().get("run_interval_seconds", 300),
            "next_run_at": self.next_run_at,
        }

    def settings(self) -> dict:
        config = load_config()
        return {
            "daily_stop_windows": config.get("daily_stop_windows", []),
            "power_mode": config.get("power_mode", "auto"),
        }

    def update_settings(self, windows: list[str], power_mode: str) -> dict:
        saved_windows, saved_power_mode = save_settings(windows, power_mode)
        return {
            "daily_stop_windows": saved_windows,
            "power_mode": saved_power_mode,
            "run_result": self.run_once(),
        }

    def run_once(self) -> dict:
        config = load_config()
        validate_config(config)
        client = self._client(config)
        result = {"timestamp": int(time.time()), "action": None}

        # Collect account and traffic metrics on every run, including scheduled downtime.
        balance = self._balance(client)
        result["balance_cny"] = balance
        data_dir().mkdir(parents=True, exist_ok=True)
        balance_history = self._read_json(self.balance_file, [])
        if not isinstance(balance_history, list):
            balance_history = []
        balance_history.append({"timestamp": result["timestamp"], "available_amount": balance})
        now = datetime.fromtimestamp(result["timestamp"])
        current_month_start = now.replace(day=1, hour=0, minute=0, second=0, microsecond=0)
        previous_month_end = current_month_start - timedelta(seconds=1)
        cutoff = previous_month_end.replace(day=1, hour=0, minute=0, second=0, microsecond=0).timestamp()
        self.balance_file.write_text(
            json.dumps([item for item in balance_history if item.get("timestamp", 0) >= cutoff], ensure_ascii=False),
            encoding="utf-8",
        )

        traffic = self._traffic_gb(client)
        result["traffic_gb"] = round(traffic, 4)
        days_in_month = calendar.monthrange(datetime.now().year, datetime.now().month)[1]
        days_left = max(days_in_month - datetime.now().day + 1, 1)
        result["daily_remaining_gb"] = round(max(config["traffic_threshold_gb"] - traffic, 0) / days_left, 4)
        result["traffic_threshold_gb"] = config["traffic_threshold_gb"]
        self.metrics_file.write_text(
            json.dumps({key: result[key] for key in ("traffic_gb", "traffic_threshold_gb", "daily_remaining_gb")}),
            encoding="utf-8",
        )

        power_mode = config["power_mode"]
        # The traffic cap is a hard safety limit and always takes precedence.
        if traffic >= config["traffic_threshold_gb"]:
            result["action"] = {item["InstanceId"]: self._stop(client, item["InstanceId"]) for item in self._instances(client)}
            result["reason"] = "traffic_threshold_reached"
            result["ecs_status"] = "Stopping"
        elif power_mode == "on":
            result["action"] = self._start(client, config["ecs_instance_id"])
            result["reason"] = "forced_on"
            result["ecs_status"] = "Running" if result["action"] == "already_running" else "Starting"
        elif power_mode == "off":
            result["action"] = self._stop(client, config["ecs_instance_id"])
            result["reason"] = "forced_off"
            result["ecs_status"] = "Stopped" if result["action"] == "already_stopped" else "Stopping"
        elif self._within_stop_window(config["daily_stop_windows"]):
            result["action"] = self._stop(client, config["ecs_instance_id"])
            result["reason"] = "scheduled_stop_window"
            result["ecs_status"] = "Stopped" if result["action"] == "already_stopped" else "Stopping"
        elif balance < config["balance_threshold_cny"]:
            result["action"] = {item["InstanceId"]: self._stop(client, item["InstanceId"]) for item in self._instances(client)}
            result["reason"] = "low_balance"
        else:
            if traffic < config["traffic_threshold_gb"]:
                result["action"] = self._start(client, config["ecs_instance_id"])
                result["reason"] = "under_traffic_threshold"
                result["ecs_status"] = "Running" if result["action"] == "already_running" else "Starting"
        result["ecs_status"] = self._record_instance_state(
            client,
            config["ecs_instance_id"],
            result.get("ecs_status"),
            result.get("reason"),
        )
        self.last_result = {"status": "完成", **result}
        self._save_status(self.last_result)
        logger.info("执行完成：%s", self.last_result)
        return self.last_result
