from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


class EvaluateRSIPolicyGateTest(unittest.TestCase):
    def setUp(self) -> None:
        self.repo = Path(__file__).resolve().parents[1]

    def write_summary(
        self,
        root: Path,
        strategy: str,
        task: str,
        run: int,
        reward: float,
        cost: float,
        *,
        leakage: int = 0,
    ) -> Path:
        episode_dir = root / strategy / f"{task}-{run}"
        episode_dir.mkdir(parents=True)
        payload = {
            "schema_version": "episode-summary-v1",
            "episode_id": f"{task}-{strategy}-{run}",
            "task_name": task,
            "trial_name": f"{task}-{strategy}-{run}",
            "reward": reward,
            "total_cost_usd": cost,
            "agent_call_count": 10 + run,
            "decision_call_count": 3,
            "length_finish_count": 1,
            "episode_adjust_call_count": 0,
            "provider_incomplete_count": 0,
            "future_evidence_leakage": leakage,
            "model_counts": {
                "z-ai/glm-5.3-flash": 7,
                "anthropic/claude-opus-5": 3,
            },
            "route_outcome_labels": {
                "test_passed": 1 if reward else 0,
                "test_failed": 0 if reward else 1,
            },
        }
        path = episode_dir / "episode-summary.json"
        path.write_text(json.dumps(payload), encoding="utf-8")
        return path

    def run_gate(self, baseline: Path, candidate: Path, output: Path, *extra: str) -> dict:
        subprocess.run(
            [
                sys.executable,
                str(self.repo / "scripts" / "evaluate_rsi_policy_gate.py"),
                "--baseline-summary",
                str(baseline),
                "--candidate-summary",
                str(candidate),
                "--output",
                str(output),
                *extra,
            ],
            cwd=self.repo,
            check=True,
            text=True,
            capture_output=True,
        )
        return json.loads(output.read_text(encoding="utf-8"))

    def test_accepts_matched_quality_with_lower_cost_per_success(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for task in ("shadow-relay", "session-window-debug"):
                self.write_summary(root, "baseline", task, 1, 1.0, 10.0)
                self.write_summary(root, "candidate", task, 1, 1.0, 6.0)

            payload = self.run_gate(
                root / "baseline",
                root / "candidate",
                root / "gate.json",
                "--min-tasks",
                "2",
                "--runs-per-task",
                "1",
            )

            self.assertEqual(payload["schema_version"], "rsi-policy-gate.v1")
            self.assertEqual(payload["gate"]["status"], "accept")
            self.assertEqual(payload["candidate"]["success_count"], 2)
            self.assertEqual(payload["baseline"]["cost_per_success"], 10.0)
            self.assertEqual(payload["candidate"]["cost_per_success"], 6.0)
            self.assertEqual(payload["deltas"]["cost_per_success"], -4.0)
            self.assertEqual(payload["matched_tasks"], ["session-window-debug", "shadow-relay"])
            self.assertEqual(len(payload["input_summaries"]["baseline"]), 2)
            self.assertTrue(payload["input_summaries"]["candidate"][0]["summary_path"].endswith("episode-summary.json"))

    def test_rejects_matched_reward_regression(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for task in ("shadow-relay", "session-window-debug"):
                self.write_summary(root, "baseline", task, 1, 1.0, 10.0)
            self.write_summary(root, "candidate", "shadow-relay", 1, 0.0, 3.0)
            self.write_summary(root, "candidate", "session-window-debug", 1, 1.0, 3.0)

            payload = self.run_gate(
                root / "baseline",
                root / "candidate",
                root / "gate.json",
                "--min-tasks",
                "2",
                "--runs-per-task",
                "1",
            )

            self.assertEqual(payload["gate"]["status"], "reject")
            self.assertIn("reward_below_matched_baseline", payload["gate"]["triggered_rollbacks"])
            self.assertIn("task_reward_regression:shadow-relay", payload["gate"]["triggered_rollbacks"])

    def test_needs_more_data_when_matched_task_count_is_low(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for task in ("shadow-relay", "session-window-debug"):
                self.write_summary(root, "baseline", task, 1, 1.0, 10.0)
            self.write_summary(root, "candidate", "shadow-relay", 1, 1.0, 5.0)

            payload = self.run_gate(
                root / "baseline",
                root / "candidate",
                root / "gate.json",
                "--min-tasks",
                "2",
                "--runs-per-task",
                "1",
            )

            self.assertEqual(payload["gate"]["status"], "needs_more_data")
            self.assertFalse(payload["gate"]["sufficient_data"])
            self.assertEqual(payload["unmatched_baseline_tasks"], ["session-window-debug"])
            self.assertIn("matched_tasks_below_minimum:1/2", payload["gate"]["warnings"])

    def test_rejects_replay_evidence_coverage_regression(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for task in ("shadow-relay", "session-window-debug"):
                self.write_summary(root, "baseline", task, 1, 1.0, 10.0)
                self.write_summary(root, "candidate", task, 1, 1.0, 5.0)
            replay = root / "replay.json"
            replay.write_text(
                json.dumps(
                    {
                        "schema_version": "rsi-router-replay.v1",
                        "summary": {
                            "rows": 2,
                            "ok": 2,
                            "future_evidence_leakage": 0,
                            "reason_evidence_coverage": 0.5,
                        },
                    }
                ),
                encoding="utf-8",
            )

            payload = self.run_gate(
                root / "baseline",
                root / "candidate",
                root / "gate.json",
                "--candidate-replay",
                str(replay),
                "--min-tasks",
                "2",
                "--runs-per-task",
                "1",
            )

            self.assertEqual(payload["gate"]["status"], "reject")
            self.assertIn("reason_evidence_coverage_below_100pct", payload["gate"]["triggered_rollbacks"])
            self.assertEqual(payload["candidate_replay"]["reason_evidence_coverage"], 0.5)


if __name__ == "__main__":
    unittest.main()
