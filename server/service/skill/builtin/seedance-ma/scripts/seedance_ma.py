#!/usr/bin/env python3
"""Volcengine Ark Seedance MA CLI wrapper for AMA."""

from __future__ import annotations

import argparse
import datetime
import hashlib
import hmac
import json
import os
import pathlib
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from typing import Any, Dict, List, Optional, Tuple


SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
SKILL_ROOT = SCRIPT_DIR.parent
CONFIG_PATH = SKILL_ROOT / "assets" / "config.json"


def load_config() -> Dict[str, Any]:
    if CONFIG_PATH.exists():
        try:
            return json.loads(CONFIG_PATH.read_text(encoding="utf-8"))
        except Exception:
            return {}
    return {}


def resolve_settings(args: argparse.Namespace) -> Tuple[str, str, str, str]:
    cfg = load_config()
    api_key = (
        getattr(args, "api_key", None)
        or os.environ.get("ARK_SEEDANCE_API_KEY")
        or os.environ.get("ARK_API_KEY")
        or os.environ.get("VOLC_API_KEY")
        or ""
    ).strip()
    api_base_url = (
        getattr(args, "api_base_url", None)
        or os.environ.get("ARK_SEEDANCE_API_BASE_URL")
        or os.environ.get("ARK_BASE_URL")
        or cfg.get("api_base_url")
        or "https://ark.cn-beijing.volces.com/api/v3"
    ).strip().rstrip("/")
    agent_id = (
        getattr(args, "agent_id", None)
        or os.environ.get("ARK_SEEDANCE_AGENT_ID")
        or cfg.get("default_agent_id")
        or ""
    ).strip()
    environment_id = (
        getattr(args, "environment_id", None)
        or os.environ.get("ARK_SEEDANCE_ENVIRONMENT_ID")
        or cfg.get("default_environment_id")
        or ""
    ).strip()
    return api_key, api_base_url, agent_id, environment_id


def http_json(
    method: str,
    url: str,
    api_key: str,
    payload: Optional[Dict[str, Any]] = None,
    timeout: float = 25.0,
) -> Dict[str, Any]:
    headers = {
        "Authorization": f"Bearer {api_key}",
        "Accept": "application/json",
    }
    body_bytes: Optional[bytes] = None
    if payload is not None:
        headers["Content-Type"] = "application/json"
        body_bytes = json.dumps(payload, ensure_ascii=False).encode("utf-8")

    req = urllib.request.Request(url=url, data=body_bytes, headers=headers, method=method.upper())
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read().decode("utf-8", errors="replace")
            return json.loads(raw) if raw.strip() else {}
    except urllib.error.HTTPError as exc:
        err_body = ""
        try:
            err_body = exc.read().decode("utf-8", errors="replace")
        except Exception:
            pass
        raise RuntimeError(f"HTTP {exc.code} calling {url}: {err_body or exc.reason}") from exc
    except Exception as exc:
        raise RuntimeError(f"Request failed calling {url}: {exc}") from exc


def create_session(api_base_url: str, api_key: str, agent_id: str, environment_id: str, title: str) -> str:
    payload: Dict[str, Any] = {
        "agent": agent_id,
        "title": title[:80] if title else "Seedance MA 视频创作",
    }
    if environment_id:
        payload["environment_id"] = environment_id
    vault_raw = (os.environ.get("ARK_SEEDANCE_VAULT_IDS") or os.environ.get("ARK_VAULT_IDS") or "").strip()
    meta: Dict[str, Any] = {}
    if environment_id:
        meta["env_id"] = environment_id
    if vault_raw:
        if vault_raw.startswith("["):
            meta["vault_ids"] = vault_raw
        else:
            ids = [x.strip() for x in vault_raw.split(",") if x.strip()]
            if ids:
                meta["vault_ids"] = json.dumps(ids)
    if meta:
        payload["metadata"] = meta
    try:
        res = http_json("POST", f"{api_base_url}/sessions", api_key, payload=payload, timeout=25.0)
    except Exception:
        fallback_payload = {k: v for k, v in payload.items() if k != "metadata"}
        res = http_json("POST", f"{api_base_url}/sessions", api_key, payload=fallback_payload, timeout=25.0)
    session_id = str(res.get("id") or "").strip()
    if not session_id:
        raise RuntimeError(f"Failed to create Seedance MA session: {json.dumps(res, ensure_ascii=False)}")
    return session_id


def send_user_message(
    api_base_url: str,
    api_key: str,
    session_id: str,
    text: str,
    image_urls: Optional[List[str]] = None,
) -> Dict[str, Any]:
    # Note: Ark Managed Agents POST /sessions/{id}/events rejects {"type": "image_url"} content blocks
    # with HTTP 400 InvalidPayload. Reference image URLs are embedded directly in the text directive,
    # where Seedance MA fetches them natively.
    full_text = text
    if image_urls:
        missing = [u.strip() for u in image_urls if u.strip() and u.strip() not in full_text]
        if missing:
            full_text = f"{full_text}\n\n参考图片 URL：{', '.join(missing)}"
    content: List[Dict[str, Any]] = [{"type": "text", "text": full_text}]
    payload = {
        "events": [
            {
                "type": "user.message",
                "content": content,
            }
        ]
    }
    return http_json("POST", f"{api_base_url}/sessions/{session_id}/events", api_key, payload=payload, timeout=25.0)


def list_session_events(api_base_url: str, api_key: str, session_id: str) -> List[Dict[str, Any]]:
    events: List[Dict[str, Any]] = []
    seen_ids: set[str] = set()
    after_id = ""
    for _ in range(10):
        qs = "limit=100"
        if after_id:
            qs += f"&after={urllib.parse.quote(after_id)}"
        res = http_json("GET", f"{api_base_url}/sessions/{session_id}/events?{qs}", api_key, timeout=20.0)
        batch = res.get("data")
        if not isinstance(batch, list) or not batch:
            break
        new_count = 0
        for ev in batch:
            eid = str(ev.get("id") or "")
            if eid and eid in seen_ids:
                continue
            if eid:
                seen_ids.add(eid)
            events.append(ev)
            new_count += 1
        last_eid = str(batch[-1].get("id") or "").strip()
        if new_count == 0 or len(batch) < 100 or not last_eid or last_eid == after_id:
            break
        after_id = last_eid
    return events


def upload_video_to_tos(file_path: pathlib.Path) -> Optional[str]:
    ak = (os.environ.get("TOS_ACCESS_KEY") or os.environ.get("AMA_DATA_TOS_ACCESS_KEY") or "").strip()
    sk = (os.environ.get("TOS_SECRET_KEY") or os.environ.get("AMA_DATA_TOS_SECRET_KEY") or "").strip()
    bucket = (os.environ.get("TOS_BUCKET") or os.environ.get("AMA_DATA_TOS_BUCKET") or "").strip()
    region = (os.environ.get("TOS_REGION") or os.environ.get("AMA_DATA_TOS_REGION") or "cn-beijing").strip()
    endpoint = (os.environ.get("TOS_ENDPOINT") or os.environ.get("AMA_DATA_TOS_ENDPOINT") or f"tos-{region}.volces.com").strip()
    endpoint = re.sub(r"^https?://", "", endpoint).rstrip("/")
    if endpoint.startswith("tos-s3-"):
        endpoint = "tos-" + endpoint[len("tos-s3-"):]
    if not (ak and sk and bucket and file_path.exists()):
        return None

    host = f"{bucket}.{endpoint}"
    date_folder = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%d")
    object_key = f"seedance-videos/{date_folder}/{uuid.uuid4().hex[:10]}-{file_path.name}"
    data = file_path.read_bytes()

    now = datetime.datetime.now(datetime.timezone.utc)
    amz_date = now.strftime("%Y%m%dT%H%M%SZ")
    datestamp = now.strftime("%Y%m%d")
    service = "tos"
    content_type = "video/mp4"
    payload_hash = hashlib.sha256(data).hexdigest()

    def sign(key: bytes, msg: str) -> bytes:
        return hmac.new(key, msg.encode("utf-8"), hashlib.sha256).digest()

    def get_signature_key(key: str, date_stamp: str, region_name: str, service_name: str) -> bytes:
        k_date = sign(key.encode("utf-8"), date_stamp)
        k_region = sign(k_date, region_name)
        k_service = sign(k_region, service_name)
        return sign(k_service, "request")

    signing_key = get_signature_key(sk, datestamp, region, service)
    canonical_uri = "/" + urllib.parse.quote(object_key, safe="/-_.~")
    canonical_headers = (
        f"content-type:{content_type}\n"
        f"host:{host}\n"
        f"x-tos-content-sha256:{payload_hash}\n"
        f"x-tos-date:{amz_date}\n"
    )
    signed_headers = "content-type;host;x-tos-content-sha256;x-tos-date"
    canonical_request = f"PUT\n{canonical_uri}\n\n{canonical_headers}\n{signed_headers}\n{payload_hash}"
    algorithm = "TOS4-HMAC-SHA256"
    credential_scope = f"{datestamp}/{region}/{service}/request"
    string_to_sign = (
        f"{algorithm}\n{amz_date}\n{credential_scope}\n"
        f"{hashlib.sha256(canonical_request.encode('utf-8')).hexdigest()}"
    )
    put_sig = hmac.new(signing_key, string_to_sign.encode("utf-8"), hashlib.sha256).hexdigest()
    auth_header = f"{algorithm} Credential={ak}/{credential_scope}, SignedHeaders={signed_headers}, Signature={put_sig}"

    put_req = urllib.request.Request(
        url=f"https://{host}{canonical_uri}",
        data=data,
        method="PUT",
        headers={
            "Content-Type": content_type,
            "Host": host,
            "x-tos-content-sha256": payload_hash,
            "x-tos-date": amz_date,
            "Authorization": auth_header,
        },
    )
    try:
        with urllib.request.urlopen(put_req, timeout=45.0) as resp:
            if resp.status not in (200, 201, 204):
                return None
    except Exception:
        return None

    expires = "604800"
    public_endpoint = (
        os.environ.get("TOS_PUBLIC_ENDPOINT")
        or endpoint
    ).strip()
    public_endpoint = re.sub(r"^https?://", "", public_endpoint).rstrip("/")
    if public_endpoint.startswith("tos-s3-"):
        public_endpoint = "tos-" + public_endpoint[len("tos-s3-"):]
    public_host = f"{bucket}.{public_endpoint}"
    query_params = {
        "X-Tos-Algorithm": algorithm,
        "X-Tos-Credential": f"{ak}/{credential_scope}",
        "X-Tos-Date": amz_date,
        "X-Tos-Expires": expires,
        "X-Tos-SignedHeaders": "host",
        "response-content-type": "video/mp4",
        "response-content-disposition": "inline",
    }
    canonical_querystring = "&".join(
        f"{urllib.parse.quote(k, safe='-_.~')}={urllib.parse.quote(v, safe='-_.~')}"
        for k, v in sorted(query_params.items())
    )
    presign_canonical_req = (
        f"GET\n{canonical_uri}\n{canonical_querystring}\nhost:{public_host}\n\nhost\nUNSIGNED-PAYLOAD"
    )
    presign_sts = (
        f"{algorithm}\n{amz_date}\n{credential_scope}\n"
        f"{hashlib.sha256(presign_canonical_req.encode('utf-8')).hexdigest()}"
    )
    get_sig = hmac.new(signing_key, presign_sts.encode("utf-8"), hashlib.sha256).hexdigest()
    return f"https://{public_host}{canonical_uri}?{canonical_querystring}&X-Tos-Signature={get_sig}"


def download_and_persist_video(
    video_url: str,
    output_dir: pathlib.Path,
    filename_hint: str,
) -> Tuple[Optional[str], Optional[str], Optional[int]]:
    if not video_url:
        return None, None, None
    try:
        output_dir.mkdir(parents=True, exist_ok=True)
        safe_name = re.sub(r"[^a-zA-Z0-9._-]+", "_", filename_hint).strip("_") or "seedance_video"
        if not safe_name.endswith(".mp4"):
            safe_name += ".mp4"
        local_file = output_dir / safe_name
        req = urllib.request.Request(video_url, headers={"User-Agent": "AMA-SeedanceMA/1.0"})
        with urllib.request.urlopen(req, timeout=45.0) as resp:
            local_file.write_bytes(resp.read())
        size_bytes = local_file.stat().st_size if local_file.exists() else None
        # Also extract cover.png alongside the video so lark-cli --video --video-cover ./cover.png works directly
        if local_file.exists() and size_bytes:
            try:
                import subprocess
                cover_path = output_dir / "cover.png"
                subprocess.run(
                    ["ffmpeg", "-hide_banner", "-loglevel", "error", "-ss", "1.0", "-i", str(local_file), "-frames:v", "1", "-y", str(cover_path)],
                    timeout=15,
                    check=False,
                )
            except Exception:
                pass
        tos_url = upload_video_to_tos(local_file)
        return str(local_file), tos_url, size_bytes
    except Exception:
        return None, None, None


def analyze_events(events: List[Dict[str, Any]]) -> Dict[str, Any]:
    storyboard_prompt = ""
    ratio = ""
    duration: Optional[int] = None
    remote_output_path = ""
    task_id = ""
    cgt_task_id = ""
    ark_video_url = ""
    view_media_url = ""
    view_media_after_compose = ""
    latest_status = ""
    error_message = ""
    gen_video_called = False
    compose_video_called = False
    gen_image_count = 0
    agent_messages: List[str] = []
    messages_after_video: List[str] = []
    tool_name_by_call_id: Dict[str, str] = {}

    for ev in events:
        ev_type = str(ev.get("type") or "")
        if ev_type in (
            "session.status_running",
            "session.status_idle",
            "session.thread_status_idle",
            "session.status_error",
            "session.error",
            "error",
        ):
            latest_status = ev_type
            if ev_type in ("session.status_error", "session.error", "error"):
                err_obj = ev.get("error") or {}
                if isinstance(err_obj, dict):
                    error_message = str(err_obj.get("message") or err_obj)
                else:
                    error_message = str(err_obj or ev.get("message") or "Session error")

        elif ev_type == "agent.tool_use":
            tool_name = str(ev.get("name") or "")
            call_id = str(ev.get("id") or "")
            if call_id and tool_name:
                tool_name_by_call_id[call_id] = tool_name
            raw_input = ev.get("input")
            inp: Dict[str, Any] = {}
            if isinstance(raw_input, dict):
                inp = raw_input
            elif isinstance(raw_input, str) and raw_input.strip():
                try:
                    parsed_inp = json.loads(raw_input)
                    if isinstance(parsed_inp, dict):
                        inp = parsed_inp
                except Exception:
                    pass

            if tool_name in ("GenVideo", "video_generate"):
                gen_video_called = True
                if inp.get("prompt"):
                    storyboard_prompt = str(inp.get("prompt"))
                if inp.get("ratio"):
                    ratio = str(inp.get("ratio"))
                if inp.get("duration") is not None:
                    try:
                        duration = int(inp.get("duration"))
                    except Exception:
                        pass
                if inp.get("output_path"):
                    remote_output_path = str(inp.get("output_path"))
            elif tool_name in ("ComposeVideo", "compose_video"):
                compose_video_called = True
                if inp.get("output_path"):
                    remote_output_path = str(inp.get("output_path"))
            elif tool_name in ("GenImage", "image_generate"):
                gen_image_count += 1

        elif ev_type == "agent.tool_result":
            call_id = str(ev.get("tool_use_id") or "")
            tool_name = str(ev.get("name") or tool_name_by_call_id.get(call_id) or "")
            content_list = ev.get("content")
            if not isinstance(content_list, list):
                content_list = []

            for item in content_list:
                if not isinstance(item, dict):
                    continue
                item_type = str(item.get("type") or "")
                if item_type == "video":
                    src = item.get("source")
                    if isinstance(src, dict) and src.get("url"):
                        view_media_url = str(src["url"])
                        if compose_video_called:
                            view_media_after_compose = view_media_url
                elif item_type == "text":
                    txt = str(item.get("text") or "").strip()
                    if not txt.startswith("{"):
                        continue
                    try:
                        obj = json.loads(txt)
                    except Exception:
                        continue
                    if not isinstance(obj, dict):
                        continue
                    if obj.get("task_id") and tool_name in (
                        "GenVideo",
                        "video_generate",
                        "ComposeVideo",
                        "compose_video",
                        "task_get",
                        "",
                    ):
                        task_id = str(obj["task_id"])
                    res_obj = obj.get("result")
                    if isinstance(res_obj, dict):
                        meta = res_obj.get("meta_info")
                        if isinstance(meta, dict):
                            if meta.get("video_url"):
                                ark_video_url = str(meta["video_url"])
                            if meta.get("task_id"):
                                cgt_task_id = str(meta["task_id"])
                        if res_obj.get("isError"):
                            err_texts = [
                                str(c.get("text") or "")
                                for c in (res_obj.get("content") or [])
                                if isinstance(c, dict)
                            ]
                            error_message = "; ".join(t for t in err_texts if t) or "Video generation task error"
                    if tool_name == "task_get" and str(obj.get("status") or "").lower() in (
                        "failed",
                        "cancelled",
                        "error",
                    ):
                        if not error_message:
                            error_message = f"Upstream task status: {obj.get('status')}"

        elif ev_type == "agent.message":
            content_list = ev.get("content")
            if isinstance(content_list, list):
                parts = [
                    str(item.get("text") or "").strip()
                    for item in content_list
                    if isinstance(item, dict) and item.get("type") == "text"
                ]
                msg = "\n".join(p for p in parts if p).strip()
                msg = re.sub(r"<display>.*?</display>\s*", "", msg, flags=re.DOTALL).strip()
                if msg:
                    agent_messages.append(msg)
                    if ark_video_url or view_media_url:
                        messages_after_video.append(msg)

    is_session_idle = latest_status in ("session.status_idle", "session.thread_status_idle")
    if compose_video_called:
        best_video_url = view_media_after_compose or (view_media_url if is_session_idle else "")
    else:
        best_video_url = view_media_url or ark_video_url
    summary = messages_after_video[-1] if messages_after_video else (agent_messages[-1] if agent_messages else "")

    if error_message and not best_video_url and is_session_idle:
        stage = "failed"
        stage_label = f"视频生成失败：{error_message}"
    elif best_video_url and is_session_idle:
        stage = "completed"
        stage_label = "Seedance 视频生成完成"
    elif compose_video_called:
        stage = "video_rendering"
        stage_label = "Seedance 正在执行多镜头剪辑合成（ComposeVideo）与成片校验"
    elif task_id or ark_video_url:
        stage = "video_rendering"
        stage_label = "Seedance 正在渲染视频画面（已提交分镜任务）"
    elif gen_video_called:
        stage = "submitting_video"
        stage_label = "正在提交 Seedance 视频渲染任务"
    elif gen_image_count > 0:
        stage = "generating_keyframes"
        stage_label = f"正在生成分镜首帧参考图（已生成 {gen_image_count} 张）"
    else:
        stage = "planning_storyboard"
        stage_label = "Seedance MA 正在规划分镜脚本与运镜提示词"

    return {
        "storyboard_prompt": storyboard_prompt,
        "ratio": ratio,
        "duration": duration,
        "remote_output_path": remote_output_path,
        "task_id": task_id,
        "cgt_task_id": cgt_task_id,
        "ark_video_url": best_video_url,
        "view_media_url": view_media_url,
        "latest_status": latest_status,
        "is_session_idle": is_session_idle,
        "error_message": error_message,
        "gen_video_called": gen_video_called,
        "compose_video_called": compose_video_called,
        "gen_image_count": gen_image_count,
        "summary": summary,
        "has_summary_after_video": len(messages_after_video) > 0,
        "stage": stage,
        "stage_label": stage_label,
        "event_count": len(events),
    }


def poll_session(
    api_base_url: str,
    api_key: str,
    agent_id: str,
    environment_id: str,
    session_id: str,
    wait_seconds: float,
    output_dir: pathlib.Path,
    action_name: str,
) -> Dict[str, Any]:
    bounded_wait = max(5.0, min(float(wait_seconds), 95.0))
    start_ts = time.monotonic()
    deadline = start_ts + bounded_wait
    nudged = False
    last_info: Dict[str, Any] = {}

    while True:
        events = list_session_events(api_base_url, api_key, session_id)
        info = analyze_events(events)
        last_info = info
        now = time.monotonic()

        if info["ark_video_url"] and info.get("is_session_idle"):
            filename_hint = (
                f"composed_{session_id}"
                if info.get("compose_video_called")
                else (info["cgt_task_id"] or info["task_id"] or session_id)
            )
            local_path, tos_url, size_bytes = download_and_persist_video(
                info["ark_video_url"],
                output_dir=output_dir,
                filename_hint=filename_hint,
            )
            final_video_url = tos_url or info["ark_video_url"]
            video_model = (os.environ.get("ARK_SEEDANCE_VIDEO_MODEL") or load_config().get("video_model") or "").strip() or None
            return {
                "ok": True,
                "action": action_name,
                "data": {
                    "outcome": "completed",
                    "stage": "completed",
                    "stage_label": "Seedance MA 视频生成完成",
                    "session_id": session_id,
                    "agent_id": agent_id,
                    "environment_id": environment_id or None,
                    "task_id": info["task_id"] or None,
                    "cgt_task_id": info["cgt_task_id"] or None,
                    "video_model": video_model,
                    "storyboard_prompt": info["storyboard_prompt"] or None,
                    "ratio": info["ratio"] or "16:9",
                    "duration": info["duration"] or 5,
                    "video_url": final_video_url,
                    "ark_video_url": info["ark_video_url"],
                    "tos_video_url": tos_url,
                    "local_path": local_path,
                    "cover_path": str(output_dir / "cover.png") if (output_dir / "cover.png").exists() else None,
                    "file_size_bytes": size_bytes,
                    "summary": info["summary"] or None,
                    "elapsed_seconds": round(now - start_ts, 1),
                },
                "next": {
                    "agent_hint": (
                        "Seedance 视频已生成并下载校验完成（AMA 前端已自动在对话中内嵌渲染 HTML5 视频播放器卡片，"
                        "且已在同目录自动提取首帧封面 cover.png）。"
                        "若当前来自飞书会话，请直接 cd 到视频输出目录后运行 lark-cli im +messages-send 发送视频；"
                        "正文中直接向用户汇报视频创意定位、分镜时间轴设计与适用投放场景即可。"
                    ),
                },
            }

        if info["error_message"] and not info["ark_video_url"] and info.get("is_session_idle"):
            return {
                "ok": False,
                "action": action_name,
                "error": {
                    "code": "VIDEO_GENERATION_FAILED",
                    "message": info["error_message"],
                },
                "data": {
                    "outcome": "failed",
                    "stage": "failed",
                    "stage_label": info["stage_label"],
                    "session_id": session_id,
                    "agent_id": agent_id,
                    "environment_id": environment_id,
                    "task_id": info["task_id"] or None,
                    "storyboard_prompt": info["storyboard_prompt"] or None,
                    "summary": info["summary"] or None,
                    "elapsed_seconds": round(now - start_ts, 1),
                },
            }

        # If Seedance MA went idle without calling GenVideo (e.g. asked a question), nudge it once automatically.
        if (
            info["latest_status"] == "session.status_idle"
            and not info["gen_video_called"]
            and not info["ark_video_url"]
            and not nudged
        ):
            nudged = True
            try:
                send_user_message(
                    api_base_url,
                    api_key,
                    session_id,
                    "请不要反问或等待确认，直接按你规划的分镜方案调用 GenVideo 工具生成视频。",
                )
            except Exception:
                pass

        if now >= deadline:
            break

        sleep_sec = min(4.0, max(1.0, deadline - now))
        time.sleep(sleep_sec)

    elapsed = round(time.monotonic() - start_ts, 1)
    script_ref = sys.argv[0] or "./skills/seedance-ma/scripts/seedance_ma.py"
    next_cmd = (
        f"python3 {script_ref} watch "
        f"--session-id {session_id} --wait-seconds 85 --output-dir {output_dir}"
    )
    video_model = (os.environ.get("ARK_SEEDANCE_VIDEO_MODEL") or load_config().get("video_model") or "").strip() or None
    return {
        "ok": True,
        "action": action_name,
        "data": {
            "outcome": "in_progress",
            "stage": last_info.get("stage") or "video_rendering",
            "stage_label": last_info.get("stage_label") or "Seedance MA 视频生成中",
            "session_id": session_id,
            "agent_id": agent_id,
            "environment_id": environment_id or None,
            "task_id": last_info.get("task_id") or None,
            "cgt_task_id": last_info.get("cgt_task_id") or None,
            "video_model": video_model,
            "storyboard_prompt": last_info.get("storyboard_prompt") or None,
            "ratio": last_info.get("ratio") or None,
            "duration": last_info.get("duration") or None,
            "video_url": None,
            "summary": last_info.get("summary") or None,
            "elapsed_seconds": elapsed,
        },
        "next": {
            "command": next_cmd,
            "agent_hint": (
                "Seedance MA 视频仍在后台渲染中（outcome=in_progress）。"
                "请立即继续执行 next.command 轮询进度，直到返回 outcome=completed 并拿到 video_url 后再向用户汇报。"
            ),
        },
    }


def build_seedance_directive(
    prompt: str,
    ratio: str,
    duration: int,
    reference_images: List[str],
    multi_clip: bool,
) -> str:
    lines = [
        "请直接根据以下创作需求完成专业分镜设计，并立即调用视频生成工具产出成片（⚠️ 当前为自动化调用模式：严禁反问用户或等待用户确认分镜，请直接拍板最佳方案并调用工具执行）：",
        f"- 核心创意与画面需求：{prompt.strip()}",
        f"- 画幅比例（ratio）：{ratio}",
        f"- 目标时长（duration）：{duration} 秒",
    ]
    if reference_images:
        lines.append(f"- 参考图片 URL：{', '.join(reference_images)}（请作为首帧或全能参考保持主体/商品外观一致）")
    if multi_clip:
        lines.append(
            "- 创作模式：多镜头叙事成片（可按需生成多段分镜视频并调用 ComposeVideo 拼接为完整成片）"
        )
    else:
        lines.append(
            "- 创作模式：单段高质感成片（无需先生成分镜图，请直接编写包含时间轴与运镜细节的精准提示词并调用一次 GenVideo 生成视频）"
        )
    lines.append("视频渲染完成后，请调用 task_get 与 ViewMedia 校验画面，并输出简短的分镜设计与成片说明。")
    return "\n".join(lines)


def cmd_status(args: argparse.Namespace) -> int:
    api_key, api_base_url, agent_id, environment_id = resolve_settings(args)
    cfg = load_config()
    has_key = bool(api_key and agent_id)
    masked_key = f"{api_key[:6]}...{api_key[-4:]}" if len(api_key) > 10 else ("configured" if api_key else "")
    out = {
        "ok": has_key,
        "action": "status",
        "data": {
            "authenticated": bool(api_key),
            "masked_api_key": masked_key,
            "api_base_url": api_base_url,
            "agent_id": agent_id,
            "environment_id": environment_id,
            "video_model": os.environ.get("ARK_SEEDANCE_VIDEO_MODEL") or cfg.get("video_model") or None,
            "image_model": os.environ.get("ARK_SEEDANCE_IMAGE_MODEL") or cfg.get("image_model") or None,
            "tos_configured": bool(
                os.environ.get("TOS_ACCESS_KEY")
                and os.environ.get("TOS_SECRET_KEY")
                and os.environ.get("TOS_BUCKET")
            ),
        },
    }
    print(json.dumps(out, ensure_ascii=False, indent=2))
    return 0 if has_key else 1


def cmd_generate(args: argparse.Namespace) -> int:
    api_key, api_base_url, agent_id, environment_id = resolve_settings(args)
    if not api_key:
        print(
            json.dumps(
                {
                    "ok": False,
                    "action": "generate",
                    "error": {
                        "code": "MISSING_API_KEY",
                        "message": "未检测到 ARK_SEEDANCE_API_KEY 或 ARK_API_KEY 环境变量。",
                    },
                },
                ensure_ascii=False,
                indent=2,
            )
        )
        return 1
    if not agent_id:
        print(
            json.dumps(
                {
                    "ok": False,
                    "action": "generate",
                    "error": {
                        "code": "MISSING_AGENT_ID",
                        "message": "未配置 ARK_SEEDANCE_AGENT_ID（或 --agent-id）。",
                    },
                },
                ensure_ascii=False,
                indent=2,
            )
        )
        return 1

    prompt = (args.prompt or "").strip()
    if not prompt:
        print(
            json.dumps(
                {
                    "ok": False,
                    "action": "generate",
                    "error": {
                        "code": "INVALID_ARGUMENT",
                        "message": "--prompt 不能为空。",
                    },
                },
                ensure_ascii=False,
                indent=2,
            )
        )
        return 1

    ratio = (args.ratio or "16:9").strip()
    duration = max(4, min(int(args.duration or 5), 15))
    ref_images: List[str] = []
    if args.reference_images:
        for item in args.reference_images:
            for part in str(item).split(","):
                if part.strip():
                    ref_images.append(part.strip())

    title = (args.title or prompt[:32]).strip()
    directive = build_seedance_directive(
        prompt=prompt,
        ratio=ratio,
        duration=duration,
        reference_images=ref_images,
        multi_clip=bool(args.multi_clip),
    )

    try:
        session_id = create_session(
            api_base_url=api_base_url,
            api_key=api_key,
            agent_id=agent_id,
            environment_id=environment_id,
            title=title,
        )
        send_user_message(
            api_base_url=api_base_url,
            api_key=api_key,
            session_id=session_id,
            text=directive,
            image_urls=ref_images,
        )
        res = poll_session(
            api_base_url=api_base_url,
            api_key=api_key,
            agent_id=agent_id,
            environment_id=environment_id,
            session_id=session_id,
            wait_seconds=args.wait_seconds,
            output_dir=pathlib.Path(args.output_dir),
            action_name="generate",
        )
        if not res["data"].get("ratio"):
            res["data"]["ratio"] = ratio
        if not res["data"].get("duration"):
            res["data"]["duration"] = duration
        res["data"]["title"] = title
        res["data"]["user_prompt"] = prompt
        print(json.dumps(res, ensure_ascii=False, indent=2))
        return 0 if res.get("ok") else 1
    except Exception as exc:
        print(
            json.dumps(
                {
                    "ok": False,
                    "action": "generate",
                    "error": {
                        "code": "SEEDANCE_MA_ERROR",
                        "message": str(exc),
                    },
                },
                ensure_ascii=False,
                indent=2,
            )
        )
        return 1


def cmd_watch(args: argparse.Namespace) -> int:
    api_key, api_base_url, agent_id, environment_id = resolve_settings(args)
    if not api_key:
        print(
            json.dumps(
                {
                    "ok": False,
                    "action": "watch",
                    "error": {
                        "code": "MISSING_API_KEY",
                        "message": "未检测到 ARK_SEEDANCE_API_KEY 或 ARK_API_KEY 环境变量。",
                    },
                },
                ensure_ascii=False,
                indent=2,
            )
        )
        return 1

    session_id = (args.session_id or "").strip()
    if not session_id:
        print(
            json.dumps(
                {
                    "ok": False,
                    "action": "watch",
                    "error": {
                        "code": "INVALID_ARGUMENT",
                        "message": "--session-id 不能为空。",
                    },
                },
                ensure_ascii=False,
                indent=2,
            )
        )
        return 1

    try:
        res = poll_session(
            api_base_url=api_base_url,
            api_key=api_key,
            agent_id=agent_id,
            environment_id=environment_id,
            session_id=session_id,
            wait_seconds=args.wait_seconds,
            output_dir=pathlib.Path(args.output_dir),
            action_name="watch",
        )
        print(json.dumps(res, ensure_ascii=False, indent=2))
        return 0 if res.get("ok") else 1
    except Exception as exc:
        print(
            json.dumps(
                {
                    "ok": False,
                    "action": "watch",
                    "error": {
                        "code": "SEEDANCE_MA_ERROR",
                        "message": str(exc),
                    },
                },
                ensure_ascii=False,
                indent=2,
            )
        )
        return 1


def build_image_directive(
    prompt: str,
    ratio: str,
    count: int,
    reference_images: List[str],
) -> str:
    if count > 1:
        lines = [
            f"请作为视觉设计总监，直接调用内置的 GenImage（image_generate）工具生成一组共 {count} 张风格、版式、配色与字体高度统一的组图海报（⚠️ 当前为自动化调用模式：严禁调用 GenVideo，严禁使用 bash/python 直接请求外部接口，严禁反问用户或等待确认）：",
            f"- 组图张数：{count} 张（请依次保存到 `/mnt/session/outputs/group_01.png` 至 `/mnt/session/outputs/group_{count:02d}.png`）",
            f"- 画幅比例（ratio）：{ratio}（2K 高清）",
            "- 组图版式一致性策略：请先调用 1 次 GenImage 生成第 1 张图 `/mnt/session/outputs/group_01.png` 作为整套组图的视觉与版式母版；随后并行调用 GenImage 生成第 2 至第 "
            f"{count} 张图时，传入 `ref_image_paths=['/mnt/session/outputs/group_01.png']` 以锁定完全一致的版式网格、底色与卡片边框风格（同时将新图的标题、标签与中央画面完整替换为对应分图内容，不残留第 1 张旧文字）。",
            f"- 组图详细创意与逐张文案要求：\n{prompt.strip()}",
        ]
    else:
        lines = [
            "请直接调用内置的 GenImage（image_generate）工具生成 1 张高清视觉图片（⚠️ 严禁调用 GenVideo，严禁使用 bash 直接请求接口，严禁反问用户）：",
            "- 保存路径：`/mnt/session/outputs/group_01.png`",
            f"- 画幅比例（ratio）：{ratio}",
            f"- 画面创意与文字要求：\n{prompt.strip()}",
        ]
    if reference_images:
        lines.append(f"- 外部参考图片 URL：{', '.join(reference_images)}")
    lines.append(
        f"⚠️ 关键交付步骤：全部 {count} 张图片生成完毕后，请务必对 `/mnt/session/outputs/group_01.png`"
        + (f" 至 `/mnt/session/outputs/group_{count:02d}.png` 的每一张图" if count > 1 else "")
        + " 分别调用 `ViewMedia` 工具检视文字准确性并暴露可访问的 image URL（如有明显错字可删除重生成后再 ViewMedia），最后输出简短的组图设计总结。"
    )
    return "\n".join(lines)


def upload_image_to_tos(file_path: pathlib.Path, custom_object_key: Optional[str] = None) -> Optional[str]:
    ak = (os.environ.get("TOS_ACCESS_KEY") or os.environ.get("AMA_DATA_TOS_ACCESS_KEY") or "").strip()
    sk = (os.environ.get("TOS_SECRET_KEY") or os.environ.get("AMA_DATA_TOS_SECRET_KEY") or "").strip()
    bucket = (os.environ.get("TOS_BUCKET") or os.environ.get("AMA_DATA_TOS_BUCKET") or "").strip()
    region = (os.environ.get("TOS_REGION") or os.environ.get("AMA_DATA_TOS_REGION") or "cn-beijing").strip()
    endpoint = (os.environ.get("TOS_ENDPOINT") or os.environ.get("AMA_DATA_TOS_ENDPOINT") or f"tos-{region}.volces.com").strip()
    endpoint = re.sub(r"^https?://", "", endpoint).rstrip("/")
    if endpoint.startswith("tos-s3-"):
        endpoint = "tos-" + endpoint[len("tos-s3-"):]
    if not (ak and sk and bucket and file_path.exists()):
        return None

    host = f"{bucket}.{endpoint}"
    date_folder = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%d")
    object_key = custom_object_key or f"seedance-images/{date_folder}/{uuid.uuid4().hex[:10]}-{file_path.name}"
    data = file_path.read_bytes()

    now = datetime.datetime.now(datetime.timezone.utc)
    amz_date = now.strftime("%Y%m%dT%H%M%SZ")
    datestamp = now.strftime("%Y%m%d")
    service = "tos"
    content_type = "image/jpeg" if file_path.suffix.lower() in (".jpg", ".jpeg") else "image/png"
    payload_hash = hashlib.sha256(data).hexdigest()

    def sign(key: bytes, msg: str) -> bytes:
        return hmac.new(key, msg.encode("utf-8"), hashlib.sha256).digest()

    signing_key = sign(sign(sign(sign(sk.encode("utf-8"), datestamp), region), service), "request")
    canonical_uri = "/" + urllib.parse.quote(object_key.lstrip("/"), safe="/-_.~")
    canonical_headers = (
        f"content-type:{content_type}\n"
        f"host:{host}\n"
        f"x-tos-content-sha256:{payload_hash}\n"
        f"x-tos-date:{amz_date}\n"
    )
    signed_headers = "content-type;host;x-tos-content-sha256;x-tos-date"
    canonical_request = f"PUT\n{canonical_uri}\n\n{canonical_headers}\n{signed_headers}\n{payload_hash}"
    algorithm = "TOS4-HMAC-SHA256"
    credential_scope = f"{datestamp}/{region}/{service}/request"
    string_to_sign = (
        f"{algorithm}\n{amz_date}\n{credential_scope}\n"
        f"{hashlib.sha256(canonical_request.encode('utf-8')).hexdigest()}"
    )
    put_sig = hmac.new(signing_key, string_to_sign.encode("utf-8"), hashlib.sha256).hexdigest()
    auth_header = f"{algorithm} Credential={ak}/{credential_scope}, SignedHeaders={signed_headers}, Signature={put_sig}"

    put_req = urllib.request.Request(
        url=f"https://{host}{canonical_uri}",
        data=data,
        method="PUT",
        headers={
            "Content-Type": content_type,
            "Host": host,
            "x-tos-content-sha256": payload_hash,
            "x-tos-date": amz_date,
            "Authorization": auth_header,
        },
    )
    try:
        with urllib.request.urlopen(put_req, timeout=45.0) as resp:
            if resp.status not in (200, 201, 204):
                return None
    except Exception:
        return None

    expires = "604800"
    public_endpoint = (
        os.environ.get("TOS_PUBLIC_ENDPOINT")
        or endpoint
    ).strip()
    public_endpoint = re.sub(r"^https?://", "", public_endpoint).rstrip("/")
    if public_endpoint.startswith("tos-s3-"):
        public_endpoint = "tos-" + public_endpoint[len("tos-s3-"):]
    public_host = f"{bucket}.{public_endpoint}"
    query_params = {
        "X-Tos-Algorithm": algorithm,
        "X-Tos-Credential": f"{ak}/{credential_scope}",
        "X-Tos-Date": amz_date,
        "X-Tos-Expires": expires,
        "X-Tos-SignedHeaders": "host",
        "response-content-type": content_type,
        "response-content-disposition": "inline",
    }
    canonical_querystring = "&".join(
        f"{urllib.parse.quote(k, safe='-_.~')}={urllib.parse.quote(v, safe='-_.~')}"
        for k, v in sorted(query_params.items())
    )
    presign_canonical_req = (
        f"GET\n{canonical_uri}\n{canonical_querystring}\nhost:{public_host}\n\nhost\nUNSIGNED-PAYLOAD"
    )
    presign_sts = (
        f"{algorithm}\n{amz_date}\n{credential_scope}\n"
        f"{hashlib.sha256(presign_canonical_req.encode('utf-8')).hexdigest()}"
    )
    get_sig = hmac.new(signing_key, presign_sts.encode("utf-8"), hashlib.sha256).hexdigest()
    return f"https://{public_host}{canonical_uri}?{canonical_querystring}&X-Tos-Signature={get_sig}"


def analyze_image_events(events: List[Dict[str, Any]]) -> Dict[str, Any]:
    latest_status = ""
    error_message = ""
    gen_image_count = 0
    gen_paths: List[str] = []
    view_path_by_call_id: Dict[str, str] = {}
    images_by_path: Dict[str, str] = {}
    fallback_urls: List[str] = []
    agent_messages: List[str] = []

    for ev in events:
        ev_type = str(ev.get("type") or "")
        if ev_type in (
            "session.status_running",
            "session.status_idle",
            "session.thread_status_idle",
            "session.status_error",
            "session.error",
            "error",
        ):
            latest_status = ev_type
            if ev_type in ("session.status_error", "session.error", "error"):
                err_obj = ev.get("error") or {}
                error_message = str(err_obj.get("message") if isinstance(err_obj, dict) else (err_obj or "Session error"))
        elif ev_type == "agent.tool_use":
            tool_name = str(ev.get("name") or "")
            call_id = str(ev.get("id") or "")
            raw_input = ev.get("input")
            inp: Dict[str, Any] = {}
            if isinstance(raw_input, dict):
                inp = raw_input
            elif isinstance(raw_input, str) and raw_input.strip():
                try:
                    parsed = json.loads(raw_input)
                    if isinstance(parsed, dict):
                        inp = parsed
                except Exception:
                    pass
            if tool_name in ("GenImage", "image_generate"):
                gen_image_count += 1
                out_p = str(inp.get("output_path") or "").strip()
                if out_p and out_p not in gen_paths:
                    gen_paths.append(out_p)
            elif tool_name in ("ViewMedia", "view_media"):
                vp = str(inp.get("path") or inp.get("file_path") or "").strip()
                if call_id and vp:
                    view_path_by_call_id[call_id] = vp
        elif ev_type == "agent.tool_result":
            call_id = str(ev.get("tool_use_id") or "")
            content_list = ev.get("content")
            if not isinstance(content_list, list):
                content_list = []
            inferred_path = view_path_by_call_id.get(call_id, "")
            found_url = ""
            for item in content_list:
                if not isinstance(item, dict):
                    continue
                if item.get("type") == "text":
                    txt = str(item.get("text") or "")
                    m_save = re.findall(r"(/mnt/session/[^\s'\"\]]+\.(?:png|jpg|jpeg|webp))", txt, flags=re.I)
                    for sp in m_save:
                        if sp not in gen_paths:
                            gen_paths.append(sp)
                        if "Successfully loaded image from" in txt:
                            inferred_path = sp
                elif item.get("type") == "image":
                    src = item.get("source")
                    if isinstance(src, dict) and src.get("url"):
                        found_url = str(src["url"])
            if found_url:
                if inferred_path:
                    images_by_path[inferred_path] = found_url
                elif found_url not in fallback_urls:
                    fallback_urls.append(found_url)
        elif ev_type == "agent.message":
            content_list = ev.get("content")
            if isinstance(content_list, list):
                parts = [
                    str(item.get("text") or "").strip()
                    for item in content_list
                    if isinstance(item, dict) and item.get("type") == "text"
                ]
                msg = "\n".join(p for p in parts if p).strip()
                msg = re.sub(r"<display>.*?</display>\s*", "", msg, flags=re.DOTALL).strip()
                if msg:
                    agent_messages.append(msg)

    ordered_items: List[Tuple[str, str]] = []
    for p in sorted(images_by_path.keys()):
        ordered_items.append((p, images_by_path[p]))
    for idx, u in enumerate(fallback_urls, start=len(ordered_items) + 1):
        ordered_items.append((f"image_{idx:02d}.png", u))

    is_session_idle = latest_status in ("session.status_idle", "session.thread_status_idle")
    return {
        "latest_status": latest_status,
        "is_session_idle": is_session_idle,
        "error_message": error_message,
        "gen_image_count": gen_image_count,
        "gen_paths": gen_paths,
        "images_by_path": images_by_path,
        "ordered_items": ordered_items,
        "summary": agent_messages[-1] if agent_messages else "",
    }


def poll_image_session(
    api_base_url: str,
    api_key: str,
    agent_id: str,
    environment_id: str,
    session_id: str,
    expected_count: int,
    wait_seconds: float,
    output_dir: pathlib.Path,
    action_name: str,
) -> Dict[str, Any]:
    bounded_wait = max(5.0, min(float(wait_seconds), 240.0))
    start_ts = time.monotonic()
    deadline = start_ts + bounded_wait
    nudged_view = False
    nudged_gen = False
    last_info: Dict[str, Any] = {}

    while True:
        events = list_session_events(api_base_url, api_key, session_id)
        info = analyze_image_events(events)
        last_info = info
        now = time.monotonic()

        items = info["ordered_items"]
        missing_views = [p for p in info["gen_paths"] if p not in info["images_by_path"]]

        if info["is_session_idle"] and len(items) >= expected_count and not missing_views:
            output_dir.mkdir(parents=True, exist_ok=True)
            saved_images: List[Dict[str, Any]] = []
            for idx, (remote_path, ark_url) in enumerate(items[:expected_count], start=1):
                base_name = pathlib.Path(remote_path).name or f"group_{idx:02d}.png"
                local_file = output_dir / base_name
                size_bytes: Optional[int] = None
                tos_url: Optional[str] = None
                try:
                    req = urllib.request.Request(ark_url, headers={"User-Agent": "AMA-SeedanceMA/1.0"})
                    with urllib.request.urlopen(req, timeout=45.0) as resp:
                        local_file.write_bytes(resp.read())
                    size_bytes = local_file.stat().st_size
                    tos_url = upload_image_to_tos(local_file)
                except Exception:
                    pass
                saved_images.append(
                    {
                        "index": idx,
                        "remote_path": remote_path,
                        "local_path": str(local_file) if local_file.exists() else None,
                        "image_url": tos_url or ark_url,
                        "ark_image_url": ark_url,
                        "tos_image_url": tos_url,
                        "file_size_bytes": size_bytes,
                    }
                )
            image_model = (os.environ.get("ARK_SEEDANCE_IMAGE_MODEL") or load_config().get("image_model") or "").strip() or None
            return {
                "ok": True,
                "action": action_name,
                "data": {
                    "outcome": "completed",
                    "stage": "completed",
                    "stage_label": f"Seedance MA 组图/生图已完成（共 {len(saved_images)} 张）",
                    "session_id": session_id,
                    "agent_id": agent_id,
                    "environment_id": environment_id or None,
                    "image_model": image_model,
                    "image_count": len(saved_images),
                    "images": saved_images,
                    "summary": info["summary"] or None,
                    "elapsed_seconds": round(now - start_ts, 1),
                },
            }

        if info["is_session_idle"] and missing_views and not nudged_view:
            nudged_view = True
            try:
                paths_str = "、".join(f"`{p}`" for p in missing_views)
                send_user_message(
                    api_base_url,
                    api_key,
                    session_id,
                    f"请立即对尚未检视的图片 {paths_str} 依次调用 ViewMedia 工具，以便输出可访问的图片 URL。",
                )
            except Exception:
                pass
        elif info["is_session_idle"] and info["gen_image_count"] == 0 and not nudged_gen and not info["error_message"]:
            nudged_gen = True
            try:
                send_user_message(
                    api_base_url,
                    api_key,
                    session_id,
                    "请不要反问或等待确认，直接调用 GenImage 工具完成生图并对所有生成图片调用 ViewMedia。",
                )
            except Exception:
                pass
        elif info["is_session_idle"] and len(items) > 0 and (nudged_view or not missing_views):
            # Completed with available images even if fewer than expected_count
            output_dir.mkdir(parents=True, exist_ok=True)
            saved_images = []
            for idx, (remote_path, ark_url) in enumerate(items, start=1):
                base_name = pathlib.Path(remote_path).name or f"group_{idx:02d}.png"
                local_file = output_dir / base_name
                size_bytes = None
                tos_url = None
                try:
                    req = urllib.request.Request(ark_url, headers={"User-Agent": "AMA-SeedanceMA/1.0"})
                    with urllib.request.urlopen(req, timeout=45.0) as resp:
                        local_file.write_bytes(resp.read())
                    size_bytes = local_file.stat().st_size
                    tos_url = upload_image_to_tos(local_file)
                except Exception:
                    pass
                saved_images.append(
                    {
                        "index": idx,
                        "remote_path": remote_path,
                        "local_path": str(local_file) if local_file.exists() else None,
                        "image_url": tos_url or ark_url,
                        "ark_image_url": ark_url,
                        "tos_image_url": tos_url,
                        "file_size_bytes": size_bytes,
                    }
                )
            image_model = (os.environ.get("ARK_SEEDANCE_IMAGE_MODEL") or load_config().get("image_model") or "").strip() or None
            return {
                "ok": True,
                "action": action_name,
                "data": {
                    "outcome": "completed",
                    "stage": "completed",
                    "stage_label": f"Seedance MA 组图/生图已完成（共 {len(saved_images)} 张）",
                    "session_id": session_id,
                    "agent_id": agent_id,
                    "environment_id": environment_id or None,
                    "image_model": image_model,
                    "image_count": len(saved_images),
                    "images": saved_images,
                    "summary": info["summary"] or None,
                    "elapsed_seconds": round(now - start_ts, 1),
                },
            }

        if info["error_message"] and not items and info["is_session_idle"]:
            return {
                "ok": False,
                "action": action_name,
                "error": {"code": "IMAGE_GENERATION_FAILED", "message": info["error_message"]},
                "data": {"outcome": "failed", "session_id": session_id},
            }

        if now >= deadline:
            break
        time.sleep(min(3.0, max(1.0, deadline - now)))

    elapsed = round(time.monotonic() - start_ts, 1)
    script_ref = sys.argv[0] or "./skills/seedance-ma/scripts/seedance_ma.py"
    next_cmd = (
        f"python3 {script_ref} watch-images "
        f"--session-id {session_id} --count {expected_count} --wait-seconds 85 --output-dir {output_dir}"
    )
    image_model = (os.environ.get("ARK_SEEDANCE_IMAGE_MODEL") or load_config().get("image_model") or "").strip() or None
    return {
        "ok": True,
        "action": action_name,
        "data": {
            "outcome": "in_progress",
            "stage": "generating_images",
            "stage_label": f"Seedance MA 正在调用 GenImage 生成组图（已调用 {last_info.get('gen_image_count', 0)}/{expected_count} 次）",
            "session_id": session_id,
            "agent_id": agent_id,
            "environment_id": environment_id or None,
            "image_model": image_model,
            "expected_count": expected_count,
            "gen_image_count": last_info.get("gen_image_count", 0),
            "elapsed_seconds": elapsed,
        },
        "next": {
            "command": next_cmd,
            "agent_hint": "Seedance MA 组图仍在生成中（outcome=in_progress），请立即继续执行 next.command 轮询直到 completed。",
        },
    }


def cmd_generate_images(args: argparse.Namespace) -> int:
    api_key, api_base_url, agent_id, environment_id = resolve_settings(args)
    if not api_key:
        print(json.dumps({"ok": False, "action": "generate-images", "error": {"code": "MISSING_API_KEY", "message": "未检测到 ARK_SEEDANCE_API_KEY 或 ARK_API_KEY 环境变量。"}}, ensure_ascii=False, indent=2))
        return 1
    if not agent_id:
        print(json.dumps({"ok": False, "action": "generate-images", "error": {"code": "MISSING_AGENT_ID", "message": "未配置 ARK_SEEDANCE_AGENT_ID（或 --agent-id）。"}}, ensure_ascii=False, indent=2))
        return 1
    prompt = (args.prompt or "").strip()
    if not prompt:
        print(json.dumps({"ok": False, "action": "generate-images", "error": {"code": "INVALID_ARGUMENT", "message": "--prompt 不能为空。"}}, ensure_ascii=False, indent=2))
        return 1

    ratio = (args.ratio or "16:9").strip()
    count = max(1, min(int(args.count or 1), 8))
    ref_images: List[str] = []
    if args.reference_images:
        for item in args.reference_images:
            for part in str(item).split(","):
                if part.strip():
                    ref_images.append(part.strip())

    title = (args.title or f"Seedance MA 组图创作 ({count}张)").strip()
    directive = build_image_directive(prompt=prompt, ratio=ratio, count=count, reference_images=ref_images)
    try:
        session_id = create_session(
            api_base_url=api_base_url,
            api_key=api_key,
            agent_id=agent_id,
            environment_id=environment_id,
            title=title,
        )
        send_user_message(
            api_base_url=api_base_url,
            api_key=api_key,
            session_id=session_id,
            text=directive,
            image_urls=ref_images,
        )
        res = poll_image_session(
            api_base_url=api_base_url,
            api_key=api_key,
            agent_id=agent_id,
            environment_id=environment_id,
            session_id=session_id,
            expected_count=count,
            wait_seconds=args.wait_seconds,
            output_dir=pathlib.Path(args.output_dir),
            action_name="generate-images",
        )
        print(json.dumps(res, ensure_ascii=False, indent=2))
        return 0 if res.get("ok") else 1
    except Exception as exc:
        print(json.dumps({"ok": False, "action": "generate-images", "error": {"code": "SEEDANCE_MA_ERROR", "message": str(exc)}}, ensure_ascii=False, indent=2))
        return 1


def cmd_watch_images(args: argparse.Namespace) -> int:
    api_key, api_base_url, agent_id, environment_id = resolve_settings(args)
    if not api_key:
        print(json.dumps({"ok": False, "action": "watch-images", "error": {"code": "MISSING_API_KEY", "message": "未检测到 ARK_SEEDANCE_API_KEY 或 ARK_API_KEY 环境变量。"}}, ensure_ascii=False, indent=2))
        return 1
    try:
        res = poll_image_session(
            api_base_url=api_base_url,
            api_key=api_key,
            agent_id=agent_id,
            environment_id=environment_id,
            session_id=args.session_id.strip(),
            expected_count=max(1, int(args.count or 1)),
            wait_seconds=args.wait_seconds,
            output_dir=pathlib.Path(args.output_dir),
            action_name="watch-images",
        )
        print(json.dumps(res, ensure_ascii=False, indent=2))
        return 0 if res.get("ok") else 1
    except Exception as exc:
        print(json.dumps({"ok": False, "action": "watch-images", "error": {"code": "SEEDANCE_MA_ERROR", "message": str(exc)}}, ensure_ascii=False, indent=2))
        return 1


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="seedance_ma.py",
        description="Volcengine Ark Seedance MA CLI for AMA",
    )
    parser.add_argument("--api-key", help="Override Ark API Key")
    parser.add_argument("--api-base-url", help="Override Ark API Base URL")
    parser.add_argument("--agent-id", help="Override Seedance MA Agent ID")
    parser.add_argument("--environment-id", help="Override Seedance MA Cloud Environment ID")

    sub = parser.add_subparsers(dest="subcommand", required=True)

    sub.add_parser("status", help="Check Seedance MA configuration and credential status")

    gen_p = sub.add_parser("generate", help="Create a Seedance MA video generation session and poll progress")
    gen_p.add_argument("--prompt", "-p", required=True, help="Video creative prompt / requirement")
    gen_p.add_argument("--title", "-t", default="", help="Short video title")
    gen_p.add_argument("--ratio", "-r", default="16:9", help="Aspect ratio: 16:9, 9:16, 1:1, 4:3, 3:4, 21:9")
    gen_p.add_argument("--duration", "-d", type=int, default=5, help="Video duration in seconds (4-15, default 5)")
    gen_p.add_argument(
        "--reference-images",
        action="append",
        default=[],
        help="Optional reference image URL(s) (comma-separated or repeatable)",
    )
    gen_p.add_argument(
        "--multi-clip",
        action="store_true",
        help="Allow multi-shot storyboard generation and ComposeVideo stitching",
    )
    gen_p.add_argument(
        "--wait-seconds",
        type=float,
        default=85.0,
        help="Max seconds to poll in this call (default 85, max 95)",
    )
    gen_p.add_argument(
        "--output-dir",
        default="./outputs/videos",
        help="Directory to save downloaded MP4 video",
    )

    watch_p = sub.add_parser("watch", help="Continue polling an in-progress Seedance MA session")
    watch_p.add_argument("--session-id", "-s", required=True, help="Seedance MA session ID (sesn-...)")
    watch_p.add_argument(
        "--wait-seconds",
        type=float,
        default=85.0,
        help="Max seconds to poll in this call (default 85, max 95)",
    )
    watch_p.add_argument(
        "--output-dir",
        default="./outputs/videos",
        help="Directory to save downloaded MP4 video",
    )

    gen_img_p = sub.add_parser("generate-images", help="Create a Seedance MA image / group-image (组图) generation session via GenImage")
    gen_img_p.add_argument("--prompt", "-p", required=True, help="Image or group-image creative prompt")
    gen_img_p.add_argument("--title", "-t", default="", help="Short session title")
    gen_img_p.add_argument("--ratio", "-r", default="16:9", help="Aspect ratio: 16:9, 9:16, 1:1, 4:3, 3:4")
    gen_img_p.add_argument("--count", "-n", type=int, default=1, help="Number of images in the group (1-8, default 1)")
    gen_img_p.add_argument(
        "--reference-images",
        action="append",
        default=[],
        help="Optional reference image URL(s) (comma-separated or repeatable)",
    )
    gen_img_p.add_argument(
        "--wait-seconds",
        type=float,
        default=85.0,
        help="Max seconds to poll in this call (default 85, max 240)",
    )
    gen_img_p.add_argument(
        "--output-dir",
        default="./outputs/images",
        help="Directory to save downloaded PNG/JPEG images",
    )

    watch_img_p = sub.add_parser("watch-images", help="Continue polling an in-progress Seedance MA image/group-image session")
    watch_img_p.add_argument("--session-id", "-s", required=True, help="Seedance MA session ID (sesn-...)")
    watch_img_p.add_argument("--count", "-n", type=int, default=1, help="Expected number of images")
    watch_img_p.add_argument(
        "--wait-seconds",
        type=float,
        default=85.0,
        help="Max seconds to poll in this call (default 85, max 240)",
    )
    watch_img_p.add_argument(
        "--output-dir",
        default="./outputs/images",
        help="Directory to save downloaded PNG/JPEG images",
    )

    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()
    if args.subcommand == "status":
        return cmd_status(args)
    if args.subcommand == "generate":
        return cmd_generate(args)
    if args.subcommand == "watch":
        return cmd_watch(args)
    if args.subcommand == "generate-images":
        return cmd_generate_images(args)
    if args.subcommand == "watch-images":
        return cmd_watch_images(args)
    parser.print_help()
    return 1


if __name__ == "__main__":
    sys.exit(main())
