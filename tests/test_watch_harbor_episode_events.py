from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import threading
import unittest
from collections import Counter
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path


class WatchHarborEpisodeEventsTest(unittest.TestCase):
    def setUp(self) -> None:
        self.repo = Path(__file__).resolve().parents[1]
        self.script = self.repo / "scripts" / "watch_harbor_episode_events.py"
        self.trial_dir = self.repo / "tests" / "fixtures" / "rsi" / "sample-trial"

    def test_once_dry_run_extracts_and_dedupes_harbor_events(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            events_path = tmp_path / "events.jsonl"
            state_path = tmp_path / "state.json"
            cmd = [
                sys.executable,
                str(self.script),
                "--job-dir",
                str(self.trial_dir),
                "--once",
                "--dry-run",
                "--state-file",
                str(state_path),
                "--events-jsonl",
                str(events_path),
            ]

            first = subprocess.run(cmd, cwd=self.repo, text=True, capture_output=True)
            self.assertEqual(first.returncode, 0, first.stderr)
            events = read_jsonl(events_path)
            counts = Counter(event["kind"] for event in events)

            self.assertEqual(counts["tool_call"], 4)
            self.assertEqual(counts["file_written"], 1)
            self.assertEqual(counts["test_run"], 2)
            self.assertEqual(counts["file_modified"], 1)
            self.assertEqual(counts["test_passed"], 1)
            self.assertEqual(counts["verifier_result"], 1)
            self.assertEqual({event["episode_id"] for event in events}, {"sample-task__abc123"})
            self.assertEqual({event["session_id"] for event in events}, {"sample-task__abc123__agent"})
            self.assertEqual({event["extractor_version"] for event in events}, {"harbor-episode-watcher-v1"})

            state = json.loads(state_path.read_text(encoding="utf-8"))
            self.assertEqual(len(state["seen_event_ids"]), len(events))

            second = subprocess.run(cmd, cwd=self.repo, text=True, capture_output=True)
            self.assertEqual(second.returncode, 0, second.stderr)
            self.assertEqual(read_jsonl(events_path), events)

    def test_posts_harbor_events_to_gateway_endpoint(self) -> None:
        received: list[tuple[str, dict]] = []

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self) -> None:  # noqa: N802
                length = int(self.headers.get("Content-Length", "0"))
                received.append((self.path, json.loads(self.rfile.read(length))))
                self.send_response(202)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b'{"status":"accepted"}')

            def log_message(self, format: str, *args: object) -> None:
                return

        server = HTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory() as tmp:
                completed = subprocess.run(
                    [
                        sys.executable,
                        str(self.script),
                        "--job-dir",
                        str(self.trial_dir),
                        "--once",
                        "--gateway",
                        f"http://127.0.0.1:{server.server_port}",
                        "--state-file",
                        str(Path(tmp) / "state.json"),
                    ],
                    cwd=self.repo,
                    text=True,
                    capture_output=True,
                )
        finally:
            server.shutdown()
            thread.join(timeout=5)
            server.server_close()

        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertTrue(received)
        self.assertEqual({path for path, _ in received}, {"/v1/episode-events"})
        self.assertEqual(len(received), 1)
        events = received[0][1]["events"]
        counts = Counter(event["kind"] for event in events)
        self.assertEqual(counts["test_run"], 2)
        self.assertEqual(counts["verifier_result"], 1)
        self.assertEqual({event["session_id"] for event in events}, {"sample-task__abc123__agent"})


def read_jsonl(path: Path) -> list[dict]:
    return [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines() if line.strip()]


if __name__ == "__main__":
    unittest.main()
