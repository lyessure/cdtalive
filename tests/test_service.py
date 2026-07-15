import unittest
from datetime import datetime

from app.service import CdtService


class NextStopWindowBoundaryTests(unittest.TestCase):
    def test_returns_same_day_start_before_window(self):
        now = datetime(2026, 7, 16, 19, 58, 30)

        boundary = CdtService.next_stop_window_boundary(["20:00-22:00"], now)

        self.assertEqual(boundary, datetime(2026, 7, 16, 20, 0))

    def test_returns_same_day_end_inside_window(self):
        now = datetime(2026, 7, 16, 20, 30)

        boundary = CdtService.next_stop_window_boundary(["20:00-22:00"], now)

        self.assertEqual(boundary, datetime(2026, 7, 16, 22, 0))

    def test_skips_boundary_equal_to_current_time(self):
        now = datetime(2026, 7, 16, 20, 0)

        boundary = CdtService.next_stop_window_boundary(["20:00-22:00"], now)

        self.assertEqual(boundary, datetime(2026, 7, 16, 22, 0))

    def test_handles_cross_midnight_window(self):
        now = datetime(2026, 7, 16, 23, 0)

        boundary = CdtService.next_stop_window_boundary(["22:00-07:00"], now)

        self.assertEqual(boundary, datetime(2026, 7, 17, 7, 0))

    def test_selects_earliest_boundary_from_multiple_windows(self):
        now = datetime(2026, 7, 16, 11, 0)

        boundary = CdtService.next_stop_window_boundary(
            ["20:00-22:00", "12:00-13:00"], now
        )

        self.assertEqual(boundary, datetime(2026, 7, 16, 12, 0))

    def test_returns_none_without_windows(self):
        self.assertIsNone(CdtService.next_stop_window_boundary([], datetime(2026, 7, 16)))


if __name__ == "__main__":
    unittest.main()
