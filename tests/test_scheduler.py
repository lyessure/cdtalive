import asyncio
import sys
import unittest
from datetime import datetime
from types import ModuleType
from types import SimpleNamespace
from unittest.mock import Mock, patch


class FastAPIStub:
    def __init__(self, *args, **kwargs):
        pass

    def _route(self, *args, **kwargs):
        return lambda function: function

    get = put = post = _route


class HTTPExceptionStub(Exception):
    pass


try:
    import fastapi  # noqa: F401
except ModuleNotFoundError:
    fastapi_stub = ModuleType("fastapi")
    responses_stub = ModuleType("fastapi.responses")
    fastapi_stub.Body = lambda *args, **kwargs: None
    fastapi_stub.FastAPI = FastAPIStub
    fastapi_stub.HTTPException = HTTPExceptionStub
    responses_stub.HTMLResponse = object
    sys.modules["fastapi"] = fastapi_stub
    sys.modules["fastapi.responses"] = responses_stub

from app import web


class SchedulerStopped(Exception):
    pass


class SchedulerTests(unittest.IsolatedAsyncioTestCase):
    async def test_boundary_run_does_not_reset_regular_deadline(self):
        clock = [1_000.0]
        wait_timeouts = []
        boundaries = iter(
            [datetime.fromtimestamp(1_100), datetime.fromtimestamp(2_000)]
        )
        fake_service = SimpleNamespace(
            last_result={},
            next_run_at=None,
            is_ecs_transitioning=Mock(return_value=False),
            run_once=Mock(),
            refresh_ecs_status=Mock(),
            next_stop_window_boundary=Mock(side_effect=lambda _windows: next(boundaries)),
        )
        existing_config = SimpleNamespace(exists=lambda: True)

        async def fake_wait_for(awaitable, timeout):
            awaitable.close()
            wait_timeouts.append(timeout)
            if len(wait_timeouts) == 1:
                clock[0] = 1_100.0
                raise asyncio.TimeoutError
            raise SchedulerStopped

        with (
            patch.object(web, "service", fake_service),
            patch.object(web, "config_path", return_value=existing_config),
            patch.object(
                web,
                "load_config",
                return_value={"run_interval_seconds": 300, "daily_stop_windows": ["00:00-00:01"]},
            ),
            patch.object(web.time, "time", side_effect=lambda: clock[0]),
            patch.object(web.asyncio, "wait_for", side_effect=fake_wait_for),
        ):
            with self.assertRaises(SchedulerStopped):
                await web.scheduler()

        self.assertEqual(fake_service.run_once.call_count, 2)
        self.assertEqual(wait_timeouts, [100.0, 200.0])
        self.assertEqual(fake_service.next_run_at, 1_300)


if __name__ == "__main__":
    unittest.main()
