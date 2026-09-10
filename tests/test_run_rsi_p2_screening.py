from __future__ import annotations

import json
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


class RunRSIP2ScreeningTest(unittest.TestCase):
    def setUp(self) -> None:
        self.repo = Path(__file__).resolve().parents[1]
        self.fixture = self.repo / "tests" / "fixtures" / "rsi"
        self.script = self.repo / "scripts" / "run_rsi_p2_screening.py"

    def test_screening_passes_matched_two_task_fixture(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            artifact_dir = root / "aware-v4-fixture"
            output_dir = root / "screening"
            for task in ("sample-task", "sample-task-2"):
                self.make_job(artifact_dir, f"aware-v4-fixture-all-premium-{task}-a1", task, cost_scale=1.0)
                self.make_job(artifact_dir, f"aware-v4-fixture-smart-router-{task}-a1", task, cost_scale=0.5)

            completed = subprocess.run(
                [
                    sys.executable,
                    str(self.script),
                    "--artifact-dir",
                    str(artifact_dir),
                    "--output-dir",
                    str(output_dir),
                    "--require-screening-pass",
                ],
                cwd=self.repo,
                text=True,
                capture_output=True,
            )

            self.assertEqual(completed.returncode, 0, completed.stderr)
            summary = json.loads((output_dir / "rsi-p2-screening-summary.json").read_text(encoding="utf-8"))
            self.assertEqual(summary["screening"]["status"], "repeat_for_acceptance")
            self.assertEqual(summary["screening"]["gate_status"], "accept")
            self.assertEqual(
                summary["screening"]["matched_tasks"],
                ["terminal-bench/sample-task", "terminal-bench/sample-task-2"],
            )
            self.assertEqual(summary["thresholds"]["minimum_tasks"], 2)
            self.assertEqual(summary["thresholds"]["screening_runs_per_task"], 1)
            self.assertEqual(summary["gate"]["candidate"]["success_count"], 2)
            self.assertLess(
                summary["gate"]["candidate"]["cost_per_success"],
                summary["gate"]["baseline"]["cost_per_success"],
            )
            rows = (output_dir / "rsi-p2-screening-summary.csv").read_text(encoding="utf-8")
            self.assertIn("smart-router", rows)
            self.assertIn("all-premium", rows)

    def test_screening_requires_more_matched_tasks(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            artifact_dir = root / "aware-v4-fixture"
            output_dir = root / "screening"
            self.make_job(artifact_dir, "aware-v4-fixture-all-premium-sample-task-a1", "sample-task", cost_scale=1.0)
            self.make_job(artifact_dir, "aware-v4-fixture-smart-router-sample-task-a1", "sample-task", cost_scale=0.5)

            completed = subprocess.run(
                [
                    sys.executable,
                    str(self.script),
                    "--artifact-dir",
                    str(artifact_dir),
                    "--output-dir",
                    str(output_dir),
                ],
                cwd=self.repo,
                text=True,
                capture_output=True,
            )

            self.assertEqual(completed.returncode, 0, completed.stderr)
            summary = json.loads((output_dir / "rsi-p2-screening-summary.json").read_text(encoding="utf-8"))
            self.assertEqual(summary["screening"]["status"], "needs_more_data")
            self.assertEqual(summary["screening"]["gate_status"], "needs_more_data")
            self.assertIn("matched_tasks_below_minimum:1/2", summary["screening"]["warnings"])

    def make_job(self, artifact_dir: Path, job: str, task: str, cost_scale: float) -> None:
        trial_name = f"{task}__abc123"
        trial_dir = artifact_dir / "jobs" / job / trial_name
        shutil.copytree(self.fixture / "sample-trial", trial_dir)

        result_path = trial_dir / "result.json"
        result = json.loads(result_path.read_text(encoding="utf-8"))
        result["task_name"] = f"terminal-bench/{task}"
        result["trial_name"] = trial_name
        result_path.write_text(json.dumps(result, indent=2, sort_keys=True) + "\n", encoding="utf-8")

        traces = json.loads((self.fixture / "sample-traces.json").read_text(encoding="utf-8"))
        for trace in traces["traces"]:
            trace["trial_name"] = trial_name
            trace["session_id"] = f"{trial_name}__agent"
            if trace.get("pool") != "decision-model":
                trace["cost"] = round(float(trace.get("cost") or 0) * cost_scale, 8)
        artifact_dir.mkdir(parents=True, exist_ok=True)
        (artifact_dir / f"traces-after-{job}.json").write_text(
            json.dumps(traces, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )


if __name__ == "__main__":
    unittest.main()
