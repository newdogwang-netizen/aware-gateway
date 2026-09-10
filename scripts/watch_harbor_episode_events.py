#!/usr/bin/env python3
"""Watch Harbor artifacts and post episode progress events to aware-gateway."""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any

from extract_episode_outcomes import (
    file_modified_event,
    load_trajectory,
    read_json,
    run_exception_event,
    sorted_events,
    test_event_from_ctrf,
    tool_events_from_trajectory,
    verifier_result_event,
)

WATCHER_VERSION = "harbor-episode-watcher-v1"


def main() -> None:
    args = parse_args()
    seen = load_seen(args.state_file)
    started = time.monotonic()

    while True:
        emitted = scan_and_emit(args, seen)
        save_seen(args.state_file, seen)
        print(f"episode watcher scan emitted={emitted} seen={len(seen)}", flush=True)

        if args.once:
            break
        if args.max_seconds > 0 and time.monotonic() - started >= args.max_seconds:
            break
        time.sleep(args.poll_seconds)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Poll a Harbor job/trial directory and POST new tool/test/verifier events."
    )
    parser.add_argument(
        "--job-dir",
        required=True,
        type=Path,
        help="Harbor job directory, trial directory, or path that will appear while Harbor runs.",
    )
    parser.add_argument("--gateway", default=os.getenv("AWARE_GATEWAY", "http://localhost:12026"))
    parser.add_argument("--episode-id", default=os.getenv("AWARE_EPISODE_ID", ""))
    parser.add_argument("--episode-operation", default=os.getenv("AWARE_EPISODE_OPERATION", ""))
    parser.add_argument("--session-id", default=os.getenv("AWARE_SESSION_ID", ""))
    parser.add_argument("--trial-name", default=os.getenv("AWARE_TRIAL_NAME", ""))
    parser.add_argument("--task-name", default=os.getenv("AWARE_TASK_NAME", ""))
    parser.add_argument("--state-file", type=Path)
    parser.add_argument("--events-jsonl", type=Path)
    parser.add_argument("--poll-seconds", type=float, default=2.0)
    parser.add_argument("--max-seconds", type=float, default=0)
    parser.add_argument("--once", action="store_true")
    parser.add_argument("--dry-run", action="store_true")
    return parser.parse_args()


def scan_and_emit(args: argparse.Namespace, seen: set[str]) -> int:
    trial_dirs = discover_trial_dirs(args.job_dir)
    emitted = 0
    for trial_dir in trial_dirs:
        result = safe_read_json(trial_dir / "result.json") or {}
        episode_id = infer_episode_id(args, trial_dir, result)
        events = collect_trial_events(trial_dir, result, episode_id)
        batch: list[dict[str, Any]] = []
        for event in sorted_events(events):
            enrich_event(event, args, episode_id, result)
            event_id = event.get("event_id") or ""
            if not event_id or event_id in seen:
                continue
            batch.append(event)
        if not batch:
            continue
        posted = True
        if not args.dry_run:
            posted = post_events(gateway_event_url(args.gateway), batch, args, episode_id)
        if not posted:
            continue
        for event in batch:
            event_id = event.get("event_id") or ""
            if args.events_jsonl:
                append_jsonl(args.events_jsonl, event)
            seen.add(event_id)
            emitted += 1
    return emitted


def discover_trial_dirs(root: Path) -> list[Path]:
    if not root.exists():
        return []
    if (root / "agent").is_dir() or (root / "result.json").exists():
        return [root]

    dirs: set[Path] = set()
    for path in root.glob("*/agent"):
        if path.is_dir():
            dirs.add(path.parent)
    for path in root.glob("*/result.json"):
        if path.is_file():
            dirs.add(path.parent)
    return sorted(dirs)


def collect_trial_events(trial_dir: Path, result: dict[str, Any], episode_id: str) -> list[dict[str, Any]]:
    events: list[dict[str, Any]] = []

    trajectory = safe_load_trajectory(trial_dir / "agent")
    if trajectory:
        events.extend(tool_events_from_trajectory(episode_id, trajectory))

    patch_path = trial_dir / "artifacts" / "tmp" / "agent.patch"
    if patch_path.exists() and patch_path.stat().st_size > 0 and result:
        events.append(file_modified_event(episode_id, patch_path, result, []))

    ctrf_path = trial_dir / "verifier" / "ctrf.json"
    ctrf = safe_read_json(ctrf_path) if ctrf_path.exists() else None
    if ctrf:
        event = test_event_from_ctrf(episode_id, ctrf, result, ctrf_path)
        if event:
            events.append(event)

    verifier = verifier_result_event(episode_id, result, ctrf_path) if result else None
    if verifier:
        events.append(verifier)

    exception = result.get("exception_info") if result else None
    if exception:
        events.append(run_exception_event(episode_id, result, exception))

    return events


def safe_load_trajectory(agent_dir: Path) -> list[dict[str, Any]]:
    if not agent_dir.exists():
        return []
    try:
        return load_trajectory(agent_dir)
    except (json.JSONDecodeError, OSError):
        return []


def safe_read_json(path: Path) -> Any:
    try:
        return read_json(path)
    except (FileNotFoundError, json.JSONDecodeError, OSError):
        return None


def infer_episode_id(args: argparse.Namespace, trial_dir: Path, result: dict[str, Any]) -> str:
    if args.episode_id:
        return args.episode_id
    if result.get("trial_name"):
        return str(result["trial_name"])
    return trial_dir.name


def enrich_event(
    event: dict[str, Any],
    args: argparse.Namespace,
    episode_id: str,
    result: dict[str, Any],
) -> None:
    event.setdefault("schema_version", "event-schema-v1")
    event["episode_id"] = episode_id
    event.setdefault("episode_operation", args.episode_operation)
    event.setdefault("session_id", args.session_id or f"{episode_id}__agent")
    event.setdefault("trial_name", args.trial_name or str(result.get("trial_name") or episode_id))
    event.setdefault("task_name", args.task_name or str(result.get("task_name") or ""))
    event.setdefault("step_name", "")
    event.setdefault("certainty", "observed")
    event["extractor_version"] = WATCHER_VERSION


def gateway_event_url(gateway: str) -> str:
    base = gateway.rstrip("/")
    if base.endswith("/v1"):
        return base + "/episode-events"
    return base + "/v1/episode-events"


def post_events(url: str, events: list[dict[str, Any]], args: argparse.Namespace, episode_id: str) -> bool:
    headers = {"Content-Type": "application/json", "X-Episode-ID": episode_id}
    session_id = args.session_id or f"{episode_id}__agent"
    if session_id:
        headers["X-Session-ID"] = session_id
    first_event = events[0] if events else {}
    trial_name = args.trial_name or str(first_event.get("trial_name") or episode_id)
    if trial_name:
        headers["X-Trial-Name"] = trial_name
    if args.task_name:
        headers["X-Task-Name"] = args.task_name
    if args.episode_operation:
        headers["X-Episode-Operation"] = args.episode_operation

    data = json.dumps({"events": events}, ensure_ascii=False, sort_keys=True).encode("utf-8")
    request = urllib.request.Request(url, data=data, headers=headers, method="POST")
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            if response.status >= 300:
                raise RuntimeError(f"gateway returned HTTP {response.status}")
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8", errors="replace")
        event_ids = [str(event.get("event_id") or "") for event in events]
        print(f"failed to post episode event batch {event_ids}: HTTP {exc.code}: {body}", file=sys.stderr)
        return False
    except (urllib.error.URLError, RuntimeError) as exc:
        event_ids = [str(event.get("event_id") or "") for event in events]
        print(f"failed to post episode event batch {event_ids}: {exc}", file=sys.stderr)
        return False
    return True


def append_jsonl(path: Path, event: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as handle:
        handle.write(json.dumps(event, ensure_ascii=False, sort_keys=True) + "\n")


def load_seen(path: Path | None) -> set[str]:
    if path is None or not path.exists():
        return set()
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError):
        return set()
    return set(payload.get("seen_event_ids") or [])


def save_seen(path: Path | None, seen: set[str]) -> None:
    if path is None:
        return
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = {"schema_version": "harbor-episode-watcher-state-v1", "seen_event_ids": sorted(seen)}
    path.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
