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
            next_boundary_at = boundary.timestamp() if boundary else None
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


@app.get("/legacy", response_class=HTMLResponse)
def index():
    return """<!doctype html><html lang='zh-CN'><meta charset='utf-8'><meta name='viewport' content='width=device-width,initial-scale=1'><title>CDT Alive · 控制台</title>
<style>:root{--ink:#172033;--muted:#667085;--line:#e7eaf0;--surface:#fff;--blue:#2563eb;--blue-soft:#eff6ff;--green:#079669;--amber:#b54708}*{box-sizing:border-box}body{min-width:1040px;margin:0;background:#f6f8fb;color:var(--ink);font:15px/1.5 Inter,ui-sans-serif,system-ui,-apple-system,"Microsoft YaHei",sans-serif}.shell{width:1080px;margin:auto;padding:38px 0 56px}.top{display:flex;justify-content:space-between;gap:20px;align-items:flex-start;margin-bottom:30px}.brand{display:flex;align-items:center;gap:13px}.mark{width:42px;height:42px;border-radius:12px;background:linear-gradient(145deg,#3b82f6,#1d4ed8);display:grid;place-items:center;color:#fff;font-size:20px;font-weight:800;box-shadow:0 8px 18px #2563eb33}.eyebrow{font-size:12px;letter-spacing:.1em;color:var(--muted);font-weight:700}.title{font-size:25px;font-weight:750;letter-spacing:-.03em}.top-right{text-align:right}.updated{font-size:12px;color:var(--muted);background:#fff;border:1px solid var(--line);border-radius:16px;padding:5px 12px;box-shadow:0 1px 2px rgba(0,0,0,0.03);display:inline-block}.run,.save{border:0;border-radius:9px;padding:10px 15px;background:var(--blue);color:#fff;font:600 14px inherit;cursor:pointer;box-shadow:0 4px 10px #2563eb33}.run:hover,.save:hover{background:#1d4ed8}.run:disabled,.save:disabled{opacity:.65;cursor:wait}.hero{border:1px solid var(--line);background:var(--surface);border-radius:16px;padding:20px 22px;display:flex;align-items:center;justify-content:space-between;gap:20px;margin-bottom:16px}.state{display:flex;gap:13px;align-items:center}.dot{height:11px;width:11px;border-radius:50%;background:var(--green);box-shadow:0 0 0 5px #07966918}.dot.warn{background:#f79009;box-shadow:0 0 0 5px #f7900918}.state h2{font-size:17px;margin:0 0 3px}.state p{margin:0;color:var(--muted);font-size:13px}.action{font-size:13px;font-weight:650;color:var(--blue);background:var(--blue-soft);padding:7px 10px;border-radius:7px;white-space:nowrap}.grid{display:grid;grid-template-columns:repeat(4,1fr);gap:16px}.card,.panel{background:var(--surface);border:1px solid var(--line);border-radius:14px}.card{padding:19px}.label{color:var(--muted);font-weight:600;font-size:13px}.value{font-size:27px;line-height:1.25;letter-spacing:-.035em;font-weight:750;margin:11px 0 9px}.sub{font-size:12px;color:var(--muted);min-height:19px}.panel{margin-top:16px;padding:20px 22px}.panel-head{font-size:14px;font-weight:700;margin-bottom:15px}.facts{display:grid;grid-template-columns:repeat(3,1fr);gap:0}.fact{padding:0 20px;border-left:1px solid var(--line)}.fact:first-child{padding-left:0;border:0}.fact span{display:block;color:var(--muted);font-size:12px;margin-bottom:5px}.fact strong{font-size:15px}.setting-row{display:flex;align-items:end;gap:12px}.field{width:520px}.field label{display:block;font-size:12px;color:var(--muted);font-weight:600;margin-bottom:7px}.field input{width:100%;height:39px;border:1px solid #cfd5df;border-radius:8px;padding:0 11px;font:14px ui-monospace,SFMono-Regular,Consolas,monospace;color:var(--ink)}.field input:focus{outline:2px solid #bfdbfe;border-color:var(--blue)}#saveHint{font-size:13px;color:var(--muted);margin-left:5px}</style>
<main class='shell'><header class='top'><div class='brand'><div class='mark'>C</div><div><div class='eyebrow'>INFRASTRUCTURE CONSOLE</div><div class='title'>CDT Alive</div></div></div><div class='top-right'><div class='updated' id='updated'>正在加载…</div></div></header><section class='hero'><div class='state'><i class='dot' id='dot'></i><div><h2 id='stateTitle'>正在获取运行状态</h2><p id='stateText'>数据每 30 秒自动刷新</p></div></div><div class='action' id='action'>—</div></section><section class='grid'><article class='card'><div class='label'>账户可用余额</div><div class='value' id='balance'>—</div><div class='sub' id='balanceSub'>—</div></article><article class='card'><div class='label'>本月 CDT 流量</div><div class='value' id='traffic'>—</div><div class='sub' id='trafficSub'>—</div></article><article class='card'><div class='label'>ECS 当前状态</div><div class='value' id='ecs'>—</div><div class='sub' id='ecsSub'>—</div></article><article class='card'><div class='label'>实例运行时长</div><div class='value' id='runtime'>—</div><div class='sub' id='runtimeSub'>—</div></article></section><section class='panel'><div class='panel-head'>运行概览</div><div class='facts'><div class='fact'><span>当前策略</span><strong id='reason'>—</strong></div><div class='fact'><span>30 天计划停机 / 异常停机</span><strong id='stops'>—</strong></div><div class='fact'><span>最后一次成功执行</span><strong id='lastRun'>—</strong></div></div></section><section class='panel'><div class='panel-head'>定时停机设置</div><div class='setting-row'><div class='field'><label for='stopWindows'>每日停机时段（多个时段以英文逗号分隔）</label><input id='stopWindows' placeholder='例如：20:30-11:20,13:00-13:30'></div><button class='save' id='save' onclick='saveSettings()'>保存设置</button><span id='saveHint'>保存后下一轮检查生效</span></div></section></main>
<script>const $=id=>document.getElementById(id),num=(v,d=2)=>v===null||v===undefined?'—':Number(v).toFixed(d),time=t=>t?new Date(t*1000).toLocaleString('zh-CN',{hour12:false}):'暂无';const reasonMap={scheduled_stop_window:'定时停机窗口',under_traffic_threshold:'流量低于阈值，保持运行',traffic_threshold_reached:'流量达到阈值，执行停机',low_balance:'余额不足，执行停机'};async function refresh(){try{const [d,s]=await Promise.all([fetch('/api/dashboard',{cache:'no-store'}).then(x=>x.json()),fetch('/api/settings',{cache:'no-store'}).then(x=>x.json())]),r=d.last_run||{},b=d.balance||{},e=d.ecs||{},scheduled=e.scheduled_stop_active||r.reason==='scheduled_stop_window';$('stopWindows').value=(s.daily_stop_windows||[]).join(',');$('dot').className='dot '+(scheduled?'warn':'');$('stateTitle').textContent=scheduled?'定时停机中':'服务运行正常';$('stateText').textContent=scheduled?'当前处于设定的每日停机时段':'容器每 5 分钟自动检查一次';$('action').textContent=reasonMap[r.reason]||r.action||'等待首次执行';$('balance').textContent=b.available_cny===undefined?'暂无':num(b.available_cny)+' CNY';$('balanceSub').textContent=b.available_cny===undefined?'等待下一个常规检查':'24h 支出 '+num(b.spent_24h_cny)+' · 本月已消费 '+num(b.spent_current_month_cny)+' · 月估算 '+num(b.estimated_monthly_cny);$('traffic').textContent=r.traffic_gb===undefined?'暂无':num(r.traffic_gb)+' GB';$('trafficSub').textContent=r.traffic_gb===undefined?'定时停机期间不查询流量':'阈值 '+num(r.traffic_threshold_gb)+' GB · 日均可用 '+num(r.daily_remaining_gb)+' GB';$('ecs').textContent=e.state||({already_stopped:'已停止',already_running:'运行中'}[r.action])||'暂无';$('ecsSub').textContent=scheduled?'计划停机状态':'来自最近一次实例状态检查';$('runtime').textContent=e.running_days===null||e.running_days===undefined?'-':num(e.running_days)+' 天';$('runtimeSub').textContent=e.scheduled_stop_active?'停机状态持续中':'最近一次启动后的连续运行时长';$('reason').textContent=reasonMap[r.reason]||'暂无';$('stops').textContent=(e.scheduled_stops_30d??'—')+' / '+(e.unexpected_stops_30d??'—')+' 次';$('lastRun').textContent=time(r.timestamp);$('updated').textContent='数据更新时间：'+time(b.updated_at||r.timestamp)}catch(err){$('stateTitle').textContent='暂时无法获取数据';$('stateText').textContent='请稍后刷新或检查服务日志'}}async function runNow(){const b=$('run');b.disabled=true;b.textContent='正在执行…';try{await fetch('/api/run',{method:'POST'});await refresh()}finally{b.disabled=false;b.textContent='立即执行'}}async function saveSettings(){const b=$('save'),hint=$('saveHint'),windows=$('stopWindows').value.split(',').map(x=>x.trim()).filter(Boolean);b.disabled=true;hint.textContent='保存中…';try{let r=await fetch('/api/settings',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({daily_stop_windows:windows})});let d=await r.json();if(!r.ok)throw Error(d.detail||'保存失败');$('stopWindows').value=d.daily_stop_windows.join(',');hint.textContent='已保存，下一轮检查生效'}catch(e){hint.textContent=e.message}finally{b.disabled=false}}refresh();setInterval(refresh,30000)</script></html>"""


@app.get("/", response_class=HTMLResponse)
def dashboard_page():
    from pathlib import Path
    current_dir = Path(__file__).parent
    if not config_path().exists():
        return (current_dir / "init.html").read_text(encoding="utf-8")
    return (current_dir / "dashboard.html").read_text(encoding="utf-8")
