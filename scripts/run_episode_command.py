#!/usr/bin/env python3
"""Run a command and report its outcome as online episode events."""

from __future__ import annotations

import argparse
import json
import os
import shlex
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

from extract_episode_outcomes import (
    classify_command,
    classify_tool_result,
    compact_text,
    extract_written_paths,
    is_delivery_path,
    is_workspace_path,
)

EVENT_SCHEMA_VERSION = "event-schema-v1"
EXTRACTOR_VERSION = "online-command-runner-v1"


def main() -> None:
    args = parse_args()
    command = command_from_args(args.command)
    if not command:
        raise SystemExit("command is required after --")

    started = datetime.now(timezone.utc)
    result = run_command(command, args.cwd, args.timeout_seconds)
    ended = datetime.now(timezone.utc)

    if result.stdout:
        sys.stdout.write(result.stdout)
        sys.stdout.flush()
    if result.stderr:
        sys.stderr.write(result.stderr)
        sys.stderr.flush()

    episode_id = args.episode_id or os.getenv("AWARE_EPISODE_ID") or os.getenv("AWARE_SESSION_ID")
    if not episode_id:
        raise SystemExit("--episode-id or AWARE_EPISODE_ID is required")

    events = build_events(args, command, result, episode_id, started, ended)
    if args.events_jsonl:
        write_jsonl(args.events_jsonl, events)
    if not args.dry_run:
        post_events(gateway_event_url(args.gateway), events, args)

    if result.timed_out:
        raise SystemExit(124)
    raise SystemExit(result.returncode)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Run a shell command and POST tool/test/file outcome events to aware-gateway."
    )
    parser.add_argument("--gateway", default=os.getenv("AWARE_GATEWAY", "http://localhost:12026"))
    parser.add_argument("--episode-id", default=os.getenv("AWARE_EPISODE_ID", ""))
    parser.add_argument("--episode-operation", default=os.getenv("AWARE_EPISODE_OPERATION", ""))
    parser.add_argument("--session-id", default=os.getenv("AWARE_SESSION_ID", ""))
    parser.add_argument("--trial-name", default=os.getenv("AWARE_TRIAL_NAME", ""))
    parser.add_argument("--step-name", default=os.getenv("AWARE_STEP_NAME", ""))
    parser.add_argument("--task-name", default=os.getenv("AWARE_TASK_NAME", ""))
    parser.add_argument("--source", default="online-command-runner")
    parser.add_argument("--cwd", type=Path, default=None)
    parser.add_argument("--timeout-seconds", type=float, default=0)
    parser.add_argument("--dry-run", action="store_true", help="Run the command and write events, but do not POST.")
    parser.add_argument("--events-jsonl", type=Path, help="Optional path to append generated event JSONL.")
    parser.add_argument("command", nargs=argparse.REMAINDER)
    return parser.parse_args()


class CommandResult:
    def __init__(
        self,
        returncode: int,
        stdout: str,
        stderr: str,
        duration_s: float,
        timed_out: bool = False,
        exception: str = "",
    ) -> None:
        self.returncode = returncode
        self.stdout = stdout
        self.stderr = stderr
        self.duration_s = duration_s
        self.timed_out = timed_out
        self.exception = exception


def command_from_args(raw: list[str]) -> str:
    if raw and raw[0] == "--":
        raw = raw[1:]
    return shlex.join(raw).strip()


def run_command(command: str, cwd: Path | None, timeout_seconds: float) -> CommandResult:
    started = time.monotonic()
    timeout = timeout_seconds if timeout_seconds > 0 else None
    try:
        completed = subprocess.run(
            command,
            shell=True,
            cwd=str(cwd) if cwd else None,
            text=True,
            capture_output=True,
            timeout=timeout,
        )
        return CommandResult(
            completed.returncode,
            completed.stdout or "",
            completed.stderr or "",
            time.monotonic() - started,
        )
    except subprocess.TimeoutExpired as exc:
        return CommandResult(
            124,
            decode_timeout_output(exc.stdout),
            decode_timeout_output(exc.stderr),
            time.monotonic() - started,
            timed_out=True,
            exception=f"timeout after {timeout_seconds:g}s",
        )
    except OSError as exc:
        return CommandResult(
            127,
            "",
            str(exc),
            time.monotonic() - started,
            exception=str(exc),
        )


def decode_timeout_output(value: Any) -> str:
    if value is None:
        return ""
    if isinstance(value, bytes):
        return value.decode(errors="replace")
    return str(value)


def build_events(
    args: argparse.Namespace,
    command: str,
    result: CommandResult,
    episode_id: str,
    started: datetime,
    ended: datetime,
) -> list[dict[str, Any]]:
    output = result.stdout + result.stderr
    written_paths = detect_written_paths(command)
    command_kind = classify_command("exec_command", command)
    if written_paths and command_kind in ("unknown", "execution_probe"):
        command_kind = "file_write"
    result_class, fingerprint = classify_tool_result(output)
    if result.returncode != 0 and result_class != "failure":
        result_class = "failure"
        fingerprint = result.exception or f"exit_code_{result.returncode}"
    if result.returncode == 0 and result_class == "unknown":
        result_class = "success"
    events: list[dict[str, Any]] = []

    tool_event = base_event(args, episode_id, "tool_call", started, 0)
    tool_event["observation"] = {
        "function_name": "exec_command",
        "command_kind": command_kind,
        "command": command,
        "command_preview": compact_text(command, 320),
        "cwd": str(args.cwd or ""),
        "exit_code": result.returncode,
        "duration_s": round(result.duration_s, 4),
        "timed_out": result.timed_out,
        "output_chars": len(output),
        "output_preview": compact_text(output, 360),
        "result_class": result_class,
        "failure_fingerprint": fingerprint,
        "written_paths": written_paths,
    }
    tool_event["evidence_refs"] = ["command:stdout"] if output else ["command:exit_code"]
    events.append(tool_event)

    if written_paths:
        write_event = base_event(args, episode_id, "file_written", started + timedelta(milliseconds=1), 1)
        write_event["observation"] = {
            "target_paths": written_paths,
            "target_paths_summary": written_paths[:8],
            "delivery_target": any(is_delivery_path(path) for path in written_paths),
            "workspace_target": any(is_workspace_path(path) for path in written_paths),
            "command_preview": compact_text(command, 320),
        }
        write_event["evidence_refs"] = [tool_event["event_id"]]
        events.append(write_event)

    if command_kind in ("test", "validation"):
        test_event = base_event(args, episode_id, "test_run", ended, len(events))
        test_event["observation"] = {
            "command_kind": command_kind,
            "command": command,
            "outcome": test_outcome(result, result_class, command_kind),
            "exit_code": result.returncode,
            "failure_fingerprint": fingerprint,
        }
        test_event["evidence_refs"] = [tool_event["event_id"]]
        events.append(test_event)

    if result.exception:
        exception_event = base_event(args, episode_id, "run_exception", ended, len(events))
        exception_event["observation"] = {
            "message": result.exception,
            "exit_code": result.returncode,
            "timed_out": result.timed_out,
        }
        exception_event["evidence_refs"] = [tool_event["event_id"]]
        events.append(exception_event)

    return events


def base_event(
    args: argparse.Namespace,
    episode_id: str,
    kind: str,
    timestamp: datetime,
    sequence: int,
) -> dict[str, Any]:
    return {
        "schema_version": EVENT_SCHEMA_VERSION,
        "event_id": str(uuid.uuid4()),
        "episode_id": episode_id,
        "episode_operation": args.episode_operation,
        "timestamp": timestamp.isoformat(),
        "timestamp_source": "command_runner_clock",
        "sequence": sequence,
        "kind": kind,
        "source": args.source,
        "observation": {},
        "evidence_refs": [],
        "certainty": "observed",
        "extractor_version": EXTRACTOR_VERSION,
        "session_id": args.session_id,
        "trial_name": args.trial_name,
        "step_name": args.step_name,
        "task_name": args.task_name,
    }


def test_outcome(result: CommandResult, result_class: str, command_kind: str) -> str:
    if result.returncode != 0 or result_class == "failure":
        return "failed"
    if result_class == "success" or command_kind == "validation":
        return "passed"
    return "unknown"


def detect_written_paths(command: str) -> list[str]:
    paths = extract_written_paths(command)
    if paths:
        return paths
    try:
        expanded = " ".join(shlex.split(command))
    except ValueError:
        return []
    return extract_written_paths(expanded)


def gateway_event_url(gateway: str) -> str:
    base = gateway.rstrip("/")
    if base.endswith("/v1"):
        return base + "/episode-events"
    return base + "/v1/episode-events"


def post_events(url: str, events: list[dict[str, Any]], args: argparse.Namespace) -> None:
    if not events:
        return
    headers = {"Content-Type": "application/json"}
    if args.episode_id:
        headers["X-Episode-ID"] = args.episode_id
    if args.episode_operation:
        headers["X-Episode-Operation"] = args.episode_operation
    if args.session_id:
        headers["X-Session-ID"] = args.session_id
    if args.trial_name:
        headers["X-Trial-Name"] = args.trial_name
    if args.step_name:
        headers["X-Step-Name"] = args.step_name
    if args.task_name:
        headers["X-Task-Name"] = args.task_name

    event_ids = [str(event.get("event_id") or "") for event in events]
    data = json.dumps({"events": events}, sort_keys=True).encode("utf-8")
    request = urllib.request.Request(url, data=data, headers=headers, method="POST")
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            if response.status >= 300:
                raise RuntimeError(f"gateway returned HTTP {response.status}")
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8", errors="replace")
        raise SystemExit(f"failed to post episode event batch {event_ids}: HTTP {exc.code}: {body}") from exc
    except urllib.error.URLError as exc:
        raise SystemExit(f"failed to post episode event batch {event_ids}: {exc}") from exc


def write_jsonl(path: Path, rows: list[dict[str, Any]]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as handle:
        for row in rows:
            handle.write(json.dumps(row, ensure_ascii=False, sort_keys=True) + "\n")


if __name__ == "__main__":
    main()
