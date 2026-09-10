from __future__ import annotations

import csv
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


class AnalyzeV3ResultsTest(unittest.TestCase):
    def setUp(self) -> None:
        self.repo = Path(__file__).resolve().parents[1]
        self.script = self.repo / "scripts" / "analyze_v3_results.py"

    def test_gateway_stop_gate_marker_becomes_analysis_row(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            artifact_dir = root / "aware-v4-fixture"
            job = "aware-v4-fixture-smart-router-sample-task-a1"
            job_dir = artifact_dir / "jobs" / job
            job_dir.mkdir(parents=True)
            session_id = "sample-task__abc123__agent"

            (job_dir / "gateway-stop-gate.json").write_text(
                json.dumps(
                    {
                        "job": job,
                        "task": "sample-task",
                        "attempt": "1",
                        "strategy": "smart-router",
                        "gateway_model": "auto",
                        "harbor_model": "openai/auto",
                        "session_id": session_id,
                        "trial_name": "sample-task__abc123",
                        "failure_kind": "gateway_cost_stop_gate",
                        "error_kind": "gateway_cost_stop_gate",
                        "detected_at": "2026-09-10T10:00:00Z",
                    },
                    indent=2,
                    sort_keys=True,
                )
                + "\n",
                encoding="utf-8",
            )
            traces_path = artifact_dir / f"traces-gateway-stop-{job}.json"
            traces_path.write_text(
                json.dumps(
                    {
                        "traces": [
                            {
                                "trace_id": "trace-stop-1",
                                "timestamp": "2026-09-10T09:59:59Z",
                                "model": "auto",
                                "routed_model": "auto",
                                "pool": "local",
                                "status": 409,
                                "error_kind": "gateway_cost_stop_gate",
                                "route_budget_action": "stop_trial",
                                "routing_reason": (
                                    "smart-router safe-control: decision_source=rule "
                                    "rule_id=episode_cost_without_verifier_stop_gate "
                                    "action=stop_trial"
                                ),
                                "session_id": session_id,
                                "cost": 0,
                                "prompt_tokens": 0,
                                "completion_tokens": 0,
                                "total_tokens": 0,
                            }
                        ]
                    },
                    indent=2,
                    sort_keys=True,
                )
                + "\n",
                encoding="utf-8",
            )
            config_path = root / "gateway.yaml"
            config_path.write_text("pricing:\n  enabled: true\n  models: {}\n", encoding="utf-8")
            output_path = root / "analysis.csv"

            completed = subprocess.run(
                [
                    sys.executable,
                    str(self.script),
                    "--artifact-dir",
                    str(artifact_dir),
                    "--gateway-config",
                    str(config_path),
                    "--traces-json",
                    str(traces_path),
                    "--job-glob",
                    job,
                    "--expected-rows",
                    "1",
                    "--strict",
                    "--output",
                    str(output_path),
                ],
                cwd=self.repo,
                text=True,
                capture_output=True,
            )

            self.assertEqual(completed.returncode, 0, completed.stderr)
            with output_path.open(newline="", encoding="utf-8") as handle:
                rows = list(csv.DictReader(handle))
            self.assertEqual(len(rows), 1)
            row = rows[0]
            self.assertEqual(row["failure_kind"], "gateway_cost_stop_gate")
            self.assertEqual(row["reward"], "0.0")
            self.assertEqual(row["trace_key"], session_id)
            self.assertEqual(row["agent_call_count"], "1")
            self.assertEqual(row["route_budget_actions"], "stop_trial:1")
            self.assertEqual(row["safe_control_rule_ids"], "episode_cost_without_verifier_stop_gate:1")


if __name__ == "__main__":
    unittest.main()
