from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path


class RunEpisodeCommandTest(unittest.TestCase):
    def setUp(self) -> None:
        self.repo = Path(__file__).resolve().parents[1]
        self.script = self.repo / "scripts" / "run_episode_command.py"

    def test_dry_run_writes_tool_and_file_events(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            output_path = tmp_path / "events.jsonl"
            written_path = tmp_path / "out.txt"
            completed = subprocess.run(
                [
                    sys.executable,
                    str(self.script),
                    "--episode-id",
                    "episode-runner",
                    "--dry-run",
                    "--events-jsonl",
                    str(output_path),
                    "--",
                    "python3",
                    "-c",
                    f"from pathlib import Path; Path('{written_path}').write_text('ok')",
                ],
                cwd=self.repo,
                text=True,
                capture_output=True,
            )

            self.assertEqual(completed.returncode, 0, completed.stderr)
            self.assertEqual(written_path.read_text(encoding="utf-8"), "ok")
            events = [
                json.loads(line)
                for line in output_path.read_text(encoding="utf-8").splitlines()
                if line.strip()
            ]
            self.assertEqual([event["kind"] for event in events], ["tool_call", "file_written"])
            self.assertEqual(events[0]["episode_id"], "episode-runner")
            self.assertEqual(events[0]["observation"]["result_class"], "success")
            self.assertIn(str(written_path), events[1]["observation"]["target_paths"])

    def test_posts_test_run_event_to_gateway(self) -> None:
        received: list[dict] = []

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self) -> None:  # noqa: N802
                length = int(self.headers.get("Content-Length", "0"))
                received.append(json.loads(self.rfile.read(length)))
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
            completed = subprocess.run(
                [
                    sys.executable,
                    str(self.script),
                    "--gateway",
                    f"http://127.0.0.1:{server.server_port}",
                    "--episode-id",
                    "episode-post",
                    "--session-id",
                    "trial-post__agent",
                    "--",
                    "python3",
                    "-m",
                    "unittest",
                    "--help",
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
        self.assertEqual([event["kind"] for event in received], ["tool_call", "test_run"])
        self.assertEqual(received[0]["episode_id"], "episode-post")
        self.assertEqual(received[0]["session_id"], "trial-post__agent")
        self.assertEqual(received[1]["observation"]["outcome"], "passed")
        self.assertEqual(received[1]["observation"]["command_kind"], "test")


if __name__ == "__main__":
    unittest.main()
