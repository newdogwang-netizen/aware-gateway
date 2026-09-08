from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


class ReplayEpisodeDecisionsTest(unittest.TestCase):
    def setUp(self) -> None:
        self.repo = Path(__file__).resolve().parents[1]
        self.fixture = self.repo / "tests" / "fixtures" / "rsi"

    def build_episode_dir(self, parent: Path) -> Path:
        episode_dir = parent / "episode"
        subprocess.run(
            [
                sys.executable,
                str(self.repo / "scripts" / "extract_episode_outcomes.py"),
                "--trial-dir",
                str(self.fixture / "sample-trial"),
                "--traces-json",
                str(self.fixture / "sample-traces.json"),
                "--output-dir",
                str(episode_dir),
                "--strict",
            ],
            cwd=self.repo,
            check=True,
            text=True,
            capture_output=True,
        )
        return episode_dir

    def test_dry_run_replay_summarizes_episode_state(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            episode_dir = self.build_episode_dir(Path(tmp))
            output = Path(tmp) / "replay.json"
            subprocess.run(
                [
                    sys.executable,
                    str(self.repo / "scripts" / "replay_episode_decisions.py"),
                    "--episode-dir",
                    str(episode_dir),
                    "--output",
                    str(output),
                    "--dry-run",
                ],
                cwd=self.repo,
                check=True,
                text=True,
                capture_output=True,
            )

            payload = json.loads(output.read_text(encoding="utf-8"))
            rows = payload["replay_decisions"]
            summary = payload["summary"]

            self.assertEqual(payload["schema_version"], "rsi-router-replay.v1")
            self.assertEqual(summary["rows"], 3)
            self.assertEqual(summary["ok"], 3)
            self.assertEqual(summary["future_evidence_leakage"], 0)
            self.assertEqual(summary["reason_evidence_coverage"], 1.0)
            self.assertEqual(summary["candidate_model_mix"]["z-ai/glm-5.3-flash"], 3)
            self.assertEqual(rows[0]["original_selected_model"], "anthropic/claude-opus-5")
            self.assertEqual(rows[-1]["state_no_progress_event_count"], 1)


if __name__ == "__main__":
    unittest.main()
