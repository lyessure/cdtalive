import asyncio
import logging
import os
import time
from contextlib import asynccontextmanager

from fastapi import Body, FastAPI, HTTPException
from fastapi.responses import HTMLResponse

from .config import load_config, config_path, init_config
from .service import CdtService, TRANSITION_POLL_INTERVAL_SECONDS

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
service = CdtService()
init_event = asyncio.Event()
BOUNDARY_EXECUTION_DELAY_SECONDS = 1


async def scheduler():
    next_regular_at = None
    next_boundary_at = None
    while True:
        if not config_path().exists():
            logging.info("配置文件不存在，跳过本次定时执行")
            service.last_result = {"status": "未初始化", "error": "配置文件不存在"}
            await init_event.wait()
            init_event.clear()
            next_regular_at = None
            next_boundary_at = None
            logging.info("检测到系统已初始化，立即启动定时检查程序")
            continue

        now = time.time()
        boundary_due = next_boundary_at is not None and now >= next_boundary_at
        regular_due = next_regular_at is None or now >= next_regular_at
        run_kind = None
        if regular_due and not service.is_ecs_transitioning():
            run_kind = "regular"
        elif boundary_due:
            run_kind = "boundary"

        try:
            if run_kind:
                service.run_once()
            elif service.is_ecs_transitioning():
                service.refresh_ecs_status()
        except Exception as exc:
            logging.exception("定时执行失败: %s", exc)
            service.last_result = {"status": "失败", "error": str(exc)}

        if run_kind == "regular":
            try:
                interval = load_config()["run_interval_seconds"]
            except Exception:
                interval = 300
            next_regular_at = time.time() + interval

        try:
            config = load_config()
            boundary = service.next_stop_window_boundary(config["daily_stop_windows"])
            next_boundary_at = (
                boundary.timestamp() + BOUNDARY_EXECUTION_DELAY_SECONDS
                if boundary
                else None
            )
        except Exception as exc:
            logging.exception("计算下一次定时停机边界失败: %s", exc)
            next_boundary_at = None

        now = time.time()
        wake_at = next_boundary_at
        if service.is_ecs_transitioning():
            transition_poll_at = now + TRANSITION_POLL_INTERVAL_SECONDS
            wake_at = min(wake_at, transition_poll_at) if wake_at else transition_poll_at
        elif next_regular_at is not None:
            wake_at = min(wake_at, next_regular_at) if wake_at else next_regular_at

        # A configured service always has a regular deadline, but retain a safe
        # fallback so a malformed configuration cannot stop the scheduler.
        if wake_at is None:
            wake_at = now + 300
        service.next_run_at = int(wake_at)
        try:
            await asyncio.wait_for(init_event.wait(), timeout=max(wake_at - now, 0))
            init_event.clear()
        except asyncio.TimeoutError:
            pass


@asynccontextmanager
async def lifespan(app):
    task = asyncio.create_task(scheduler())
    yield
    task.cancel()


app = FastAPI(title="CDT Alive", lifespan=lifespan)


@app.get("/api/status")
def status():
    return service.last_result


@app.get("/api/dashboard")
def dashboard():
    if not config_path().exists():
        return {
            "last_run": {"status": "未初始化", "error": "配置文件不存在"},
            "balance": None,
            "ecs": None,
            "run_interval_seconds": 300,
            "next_run_at": None,
        }
    return service.dashboard()


@app.get("/api/settings")
def settings():
    if not config_path().exists():
        return {"daily_stop_windows": [], "power_mode": "auto"}
    return service.settings()


@app.put("/api/settings")
async def save_settings(payload: dict = Body(...)):
    if not config_path().exists():
        raise HTTPException(status_code=400, detail="配置文件不存在，请先初始化配置。")
    windows = payload.get("daily_stop_windows", [])
    power_mode = payload.get("power_mode", "auto")
    if not isinstance(windows, list) or not all(isinstance(item, str) for item in windows):
        raise HTTPException(status_code=422, detail="daily_stop_windows 必须是字符串列表")
    if not isinstance(power_mode, str):
        raise HTTPException(status_code=422, detail="power_mode 必须是字符串")
    try:
        result = service.update_settings(windows, power_mode)
        # Recompute the next stop-window boundary immediately after settings change.
        init_event.set()
        return result
    except ValueError as exc:
        raise HTTPException(status_code=422, detail=str(exc)) from exc


@app.post("/api/init")
async def initialize_config(payload: dict = Body(...)):
    if config_path().exists():
        raise HTTPException(status_code=400, detail="配置文件已存在，无法重复初始化。")
    try:
        init_config(payload)
        # Notify the scheduler loop to start immediately
        init_event.set()
        return {"status": "success"}
    except ValueError as exc:
        raise HTTPException(status_code=422, detail=str(exc)) from exc


@app.post("/api/run")
async def run_now():
    if not config_path().exists():
        raise HTTPException(status_code=400, detail="配置文件不存在，请先初始化配置。")
    try:
        result = service.run_once()
        if service.is_ecs_transitioning():
            init_event.set()
        return result
    except Exception as exc:
        raise HTTPException(status_code=500, detail=str(exc)) from exc


@app.get("/", response_class=HTMLResponse)
def dashboard_page():
    from pathlib import Path
    current_dir = Path(__file__).parent
    if not config_path().exists():
        return (current_dir / "init.html").read_text(encoding="utf-8")
    return (current_dir / "dashboard.html").read_text(encoding="utf-8")
