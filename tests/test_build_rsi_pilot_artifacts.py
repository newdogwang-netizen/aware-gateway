from __future__ import annotations

import json
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


class BuildRSIPilotArtifactsTest(unittest.TestCase):
    def setUp(self) -> None:
        self.repo = Path(__file__).resolve().parents[1]
        self.fixture = self.repo / "tests" / "fixtures" / "rsi"
        self.script = self.repo / "scripts" / "build_rsi_pilot_artifacts.py"

    def test_builds_episode_artifacts_and_policy_gate_from_v4_jobs(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            artifact_dir = root / "aware-v4-fixture"
            output_dir = root / "rsi-output"
            self.make_job(artifact_dir, "aware-v4-fixture-all-premium-sample-task-a1", cost_scale=1.0)
            self.make_job(artifact_dir, "aware-v4-fixture-smart-router-sample-task-a1", cost_scale=0.5)

            completed = subprocess.run(
                [
                    sys.executable,
                    str(self.script),
                    "--artifact-dir",
                    str(artifact_dir),
                    "--output-dir",
                    str(output_dir),
                    "--strategy",
                    "all-premium,smart-router",
                    "--baseline-strategy",
                    "all-premium",
                    "--candidate-strategy",
                    "smart-router",
                    "--strict",
                ],
                cwd=self.repo,
                text=True,
                capture_output=True,
            )

            self.assertEqual(completed.returncode, 0, completed.stderr)
            manifest = json.loads((output_dir / "rsi-pilot-artifacts-manifest.json").read_text(encoding="utf-8"))
            self.assertEqual(manifest["counts"]["episodes"], 2)
            self.assertEqual(manifest["counts"]["ok"], 2)
            self.assertEqual(manifest["policy_gate"]["status"], "ok")

            baseline = json.loads(
                (output_dir / "all-premium" / "sample-task-a1" / "episode-summary.json").read_text(encoding="utf-8")
            )
            candidate = json.loads(
                (output_dir / "smart-router" / "sample-task-a1" / "episode-summary.json").read_text(encoding="utf-8")
            )
            gate = json.loads(
                (output_dir / "policy-gate-all-premium-vs-smart-router.json").read_text(encoding="utf-8")
            )

            self.assertEqual(baseline["agent_cost_usd"], 0.73)
            self.assertEqual(baseline["decision_cost_usd"], 0.06)
            self.assertEqual(baseline["total_cost_usd"], 0.79)
            self.assertEqual(candidate["agent_cost_usd"], 0.365)
            self.assertEqual(candidate["decision_cost_usd"], 0.06)
            self.assertEqual(candidate["total_cost_usd"], 0.425)
            self.assertEqual(gate["gate"]["status"], "accept")
            self.assertEqual(gate["matched_tasks"], ["terminal-bench/sample-task"])

    def make_job(self, artifact_dir: Path, job: str, cost_scale: float) -> None:
        trial_dir = artifact_dir / "jobs" / job / "sample-task__abc123"
        shutil.copytree(self.fixture / "sample-trial", trial_dir)

        traces = json.loads((self.fixture / "sample-traces.json").read_text(encoding="utf-8"))
        for trace in traces["traces"]:
            if trace.get("pool") != "decision-model":
                trace["cost"] = round(float(trace.get("cost") or 0) * cost_scale, 8)
        artifact_dir.mkdir(parents=True, exist_ok=True)
        (artifact_dir / f"traces-after-{job}.json").write_text(
            json.dumps(traces, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )


if __name__ == "__main__":
    unittest.main()
