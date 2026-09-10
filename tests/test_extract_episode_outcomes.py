from __future__ import annotations

import json
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


class ExtractEpisodeOutcomesTest(unittest.TestCase):
    def setUp(self) -> None:
        self.repo = Path(__file__).resolve().parents[1]
        self.fixture = self.repo / "tests" / "fixtures" / "rsi"

    def run_extractor(self) -> tuple[list[dict], dict, dict]:
        with tempfile.TemporaryDirectory() as tmp:
            output_dir = Path(tmp)
            subprocess.run(
                [
                    sys.executable,
                    str(self.repo / "scripts" / "extract_episode_outcomes.py"),
                    "--trial-dir",
                    str(self.fixture / "sample-trial"),
                    "--traces-json",
                    str(self.fixture / "sample-traces.json"),
                    "--output-dir",
                    str(output_dir),
                    "--strict",
                ],
                cwd=self.repo,
                check=True,
                text=True,
                capture_output=True,
            )
            events = [
                json.loads(line)
                for line in (output_dir / "episode-events.jsonl").read_text(encoding="utf-8").splitlines()
                if line.strip()
            ]
            summary = json.loads((output_dir / "episode-summary.json").read_text(encoding="utf-8"))
            cutoff = json.loads((output_dir / "replay-cutoff-check.json").read_text(encoding="utf-8"))
            return events, summary, cutoff

    def run_extractor_without_traces(self) -> tuple[list[dict], dict, dict]:
        with tempfile.TemporaryDirectory() as tmp:
            output_dir = Path(tmp)
            subprocess.run(
                [
                    sys.executable,
                    str(self.repo / "scripts" / "extract_episode_outcomes.py"),
                    "--trial-dir",
                    str(self.fixture / "sample-trial"),
                    "--output-dir",
                    str(output_dir),
                    "--strict",
                ],
                cwd=self.repo,
                check=True,
                text=True,
                capture_output=True,
            )
            events = [
                json.loads(line)
                for line in (output_dir / "episode-events.jsonl").read_text(encoding="utf-8").splitlines()
                if line.strip()
            ]
            summary = json.loads((output_dir / "episode-summary.json").read_text(encoding="utf-8"))
            cutoff = json.loads((output_dir / "replay-cutoff-check.json").read_text(encoding="utf-8"))
            return events, summary, cutoff

    def test_extracts_auditable_events_and_summary(self) -> None:
        events, summary, _ = self.run_extractor()
        required = set(json.loads((self.repo / "docs" / "rsi" / "event-schema-v1.json").read_text())["required"])

        self.assertEqual(summary["episode_id"], "sample-task__abc123")
        self.assertEqual(summary["agent_call_count"], 6)
        self.assertEqual(summary["decision_call_count"], 3)
        self.assertEqual(summary["provider_incomplete_count"], 1)
        self.assertEqual(summary["length_finish_count"], 3)
        self.assertEqual(summary["episode_adjust_call_count"], 3)
        self.assertEqual(summary["event_counts"]["file_modified"], 1)
        self.assertEqual(summary["event_counts"]["file_written"], 1)
        self.assertEqual(summary["event_counts"]["test_passed"], 1)
        self.assertEqual(summary["event_counts"]["test_run"], 2)
        self.assertEqual(summary["event_counts"]["tool_call"], 4)
        self.assertEqual(summary["event_counts"]["verifier_result"], 1)
        self.assertEqual(summary["event_counts"]["no_progress"], 1)
        self.assertEqual(summary["delivery_file_write_count"], 1)
        self.assertEqual(summary["test_run_outcomes"], {"failed": 1, "passed": 1})
        self.assertEqual(summary["candidate_progress_event_count"], 3)
        self.assertEqual(summary["route_outcome_count"], 6)
        self.assertEqual(summary["route_outcome_labels"]["provider_incomplete"], 1)
        self.assertEqual(summary["route_outcome_labels"]["test_failed"], 1)
        self.assertEqual(summary["route_outcome_labels"]["verifier_passed"], 1)
        self.assertEqual(summary["route_outcome_with_progress_count"], 1)
        self.assertEqual(summary["reward"], 1.0)

        for event in events:
            self.assertTrue(required.issubset(event.keys()), event)
            self.assertTrue(event["evidence_refs"], event)
            self.assertIn(event["certainty"], {"observed", "derived", "inferred", "unknown"})

        outcomes = {
            (event.get("observation") or {}).get("outcome")
            for event in events
            if event["kind"] == "llm_call"
        }
        self.assertIn("response_completed", outcomes)
        self.assertIn("provider_incomplete", outcomes)
        self.assertIn("length_truncated", outcomes)
        self.assertNotIn("completed", outcomes)

    def test_replay_cutoff_excludes_future_evidence(self) -> None:
        _, _, cutoff = self.run_extractor()

        self.assertEqual(cutoff["future_evidence_leakage"], 0)
        self.assertEqual(cutoff["decision_count"], 3)
        self.assertFalse(cutoff["violations"])
        route_outcomes = {route["route_trace_id"]: route for route in cutoff["route_outcomes"]}
        self.assertEqual(route_outcomes["agent-2"]["outcome_label"], "test_failed")
        self.assertEqual(route_outcomes["agent-2"]["test_run_outcomes"], {"failed": 1})
        self.assertEqual(route_outcomes["agent-5"]["outcome_label"], "verifier_passed")
        self.assertEqual(route_outcomes["agent-5"]["verifier_reward"], 1.0)
        self.assertEqual(route_outcomes["agent-5"]["delivery_file_write_count"], 1)

        first = cutoff["samples"][0]
        self.assertEqual(first["state_before"]["llm_call_count"], 2)
        self.assertEqual(first["state_before"]["outcomes"]["provider_incomplete"], 1)
        self.assertEqual(first["post_decision_outcome"]["route_trace_id"], "agent-2")
        self.assertEqual(first["post_decision_outcome"]["outcome_label"], "test_failed")

        for sample in cutoff["samples"]:
            refs = "\n".join(sample["allowed_evidence_refs"])
            self.assertNotIn("ctrf:", refs)
            self.assertNotIn("result.json:verifier_result", refs)

        last = cutoff["samples"][-1]
        self.assertEqual(last["state_before"]["no_progress_event_count"], 1)
        self.assertEqual(last["state_before"]["no_progress_window"]["severity"], "watch")
        self.assertEqual(last["state_before"]["recent_window"]["candidate_progress_count"], 3)
        self.assertEqual(last["state_before"]["file_write_count"], 1)
        self.assertEqual(last["state_before"]["test_run_count"], 2)
        self.assertIn("agent.patch:", "\n".join(last["allowed_evidence_refs"]))

        self.assertEqual(
            first["original_decision"]["selected_model"],
            "anthropic/claude-opus-5",
        )
        self.assertEqual(
            last["original_decision"]["selected_model"],
            "",
        )
        self.assertEqual(last["post_decision_outcome"]["outcome_label"], "unpaired")

    def test_can_project_basic_llm_events_from_trajectory_without_traces(self) -> None:
        events, summary, cutoff = self.run_extractor_without_traces()
        llm_events = [event for event in events if event["kind"] == "llm_call"]

        self.assertEqual(summary["agent_call_count"], 3)
        self.assertEqual(summary["decision_call_count"], 0)
        self.assertEqual(summary["trajectory_agent_turn_count"], 3)
        self.assertEqual(summary["tool_call_count"], 4)
        self.assertEqual(summary["file_write_count"], 1)
        self.assertEqual(summary["test_run_count"], 2)
        self.assertEqual(cutoff["decision_count"], 0)
        self.assertEqual({event["source"] for event in llm_events}, {"harbor_trajectory"})
        self.assertEqual({event["observation"]["outcome"] for event in llm_events}, {"unknown"})
        self.assertTrue(all(event["evidence_refs"][0].startswith("trajectory:") for event in llm_events))

    def test_incomplete_trial_ignores_unmatched_session_traces(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            trial_dir = tmp_path / "shadow-relay__unfinished"
            shutil.copytree(self.fixture / "sample-trial", trial_dir)
            (trial_dir / "result.json").unlink()
            traces_path = tmp_path / "unmatched-traces.json"
            traces_path.write_text(
                json.dumps(
                    {
                        "traces": [
                            {
                                "trace_id": "direct-canary",
                                "timestamp": "2026-09-08T09:00:00Z",
                                "pool": "openrouter",
                                "trial_name": "aware-v4-direct-canary",
                                "session_id": "aware-v4-direct-canary__agent",
                                "status": 200,
                                "total_tokens": 1,
                            }
                        ]
                    }
                )
                + "\n",
                encoding="utf-8",
            )
            output_dir = tmp_path / "out"

            subprocess.run(
                [
                    sys.executable,
                    str(self.repo / "scripts" / "extract_episode_outcomes.py"),
                    "--trial-dir",
                    str(trial_dir),
                    "--traces-json",
                    str(traces_path),
                    "--output-dir",
                    str(output_dir),
                    "--strict",
                ],
                cwd=self.repo,
                check=True,
                text=True,
                capture_output=True,
            )

            events = [
                json.loads(line)
                for line in (output_dir / "episode-events.jsonl").read_text(encoding="utf-8").splitlines()
                if line.strip()
            ]
            summary = json.loads((output_dir / "episode-summary.json").read_text(encoding="utf-8"))
            llm_events = [event for event in events if event["kind"] == "llm_call"]

            self.assertEqual(summary["episode_id"], "shadow-relay__unfinished")
            self.assertEqual(summary["decision_call_count"], 0)
            self.assertEqual(summary["agent_call_count"], 3)
            self.assertEqual({event["source"] for event in llm_events}, {"harbor_trajectory"})


if __name__ == "__main__":
    unittest.main()
