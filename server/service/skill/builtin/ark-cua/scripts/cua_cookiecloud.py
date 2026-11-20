# Copyright (c) 2025 Beijing Volcano Engine Technology Co., Ltd. and/or its affiliates.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Private CookieCloud configuration and upload orchestration.

Configuration stays in this process and the local Chrome helper's stdin.
Only bounded domain/status metadata is returned to the CLI.
"""

import json
import re
import shutil
import subprocess
from datetime import datetime
from pathlib import Path
from urllib.parse import quote, urlsplit

from cua_util import SkillError

EXTENSION_ID = "ffjiejobkoibkjlhjnlgmcnnigeelbdl"
STORAGE_KEY = "COOKIE_SYNC_SETTING"
AUTH_CODES = {"AUTH_REQUIRED", "TOKEN_EXPIRED", "REFRESH_FAILED", "FORBIDDEN"}
RECOVERABLE_CREATE = {"NETWORK", "GATEWAY_TIMEOUT", "CUA_BACKEND_UNAVAILABLE", "CONFLICT", "Conflict", "cookiecloud_disconnect_first"}


def domains(values):
    result = []
    for value in values:
        try:
            value = value.strip().rstrip(".").encode("idna").decode("ascii").lower()
        except (AttributeError, UnicodeError):
            raise SkillError("VALIDATION_ERROR", "Select complete website domains.") from None
        if not re.fullmatch(r"(?=.{1,253}$)(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", value):
            raise SkillError("VALIDATION_ERROR", "Select complete website domains without paths or wildcards.")
        if value not in result:
            result.append(value)
    if not 1 <= len(result) <= 32:
        raise SkillError("VALIDATION_ERROR", "Select between 1 and 32 website domains.")
    return sorted(result)


def validate_cdp_url(value):
    try:
        url = urlsplit(value)
        local = url.hostname in {"localhost", "127.0.0.1", "::1"}
        valid = url.scheme in {"http", "ws"} and local and url.port and not url.username and not url.password and not url.query and not url.fragment
    except (ValueError, TypeError):
        valid = False
    if not valid:
        raise SkillError("VALIDATION_ERROR", "Use the user-authorized Chrome debugging URL on loopback.")


def _call(api, method, path, body=None):
    try:
        return api(method, path, body)
    except SkillError as exc:
        if exc.code in AUTH_CODES:
            safe = {k: v for k, v in exc.extra.items() if k in {"setup_command", "arkcli_status", "arkcli_hint", "manual_login_required"}}
            raise SkillError(exc.code, "Current Gateway authentication or desktop permission is required.", **safe) from None
        code = exc.code if exc.code in RECOVERABLE_CREATE | {"cookiecloud_not_connected"} else "COOKIECLOUD_API_UNAVAILABLE"
        raise SkillError(code, "Browser sync configuration is unavailable on this Gateway. Check its version and CookieCloud setting; keep the current environment.") from None


def _configuration(data, desktop_id):
    invalid = SkillError("COOKIECLOUD_CONFIG_INVALID", "The Gateway returned incomplete browser sync configuration; Chrome settings were not changed.")
    if not isinstance(data, dict) or data.get("desktop_id") != desktop_id or data.get("extension_id") != EXTENSION_ID or data.get("storage_key") != STORAGE_KEY:
        raise invalid
    settings = data.get("settings")
    if not isinstance(settings, dict) or any(not isinstance(settings.get(k), str) or not settings[k].strip() for k in (
        "endpoint",
        "uuid",
        "password",
        "headers",
        "domains",
    )):
        raise invalid
    try:
        endpoint = urlsplit(settings["endpoint"])
        valid_url = endpoint.scheme == "https" and endpoint.hostname and not endpoint.username and not endpoint.password and not endpoint.query and not endpoint.fragment
        allowed = domains(settings["domains"].splitlines())
    except (ValueError, SkillError):
        raise invalid from None
    if not valid_url or settings.get("type") != "up" or settings.get("with_storage") != 1 or settings.get("crypto_type") not in {"legacy", "aes-128-cbc-fixed"} or type(settings.get("interval")) is not int or settings["interval"] < 1 or (data.get("received_at") is not None and _timestamp(data["received_at"]) is None):
        raise invalid
    return {k: settings[k] for k in (
        "endpoint",
        "uuid",
        "password",
        "headers",
        "domains",
        "type",
        "with_storage",
        "interval",
        "crypto_type",
    )}, allowed


def _configure_extension(payload, timeout):
    node = shutil.which("node")
    if not node:
        raise SkillError("COOKIECLOUD_NODE_REQUIRED", "Automatic Chrome configuration needs Node.js 22 or newer; manual CUA configuration is still available.")
    try:
        version = subprocess.run([node, "--version"], text=True, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=10, check=False)
        major = re.match(r"v([0-9]+)\.", version.stdout.strip())
        if version.returncode or not major or int(major[1]) < 22:
            raise SkillError("COOKIECLOUD_NODE_REQUIRED", "Automatic Chrome configuration needs Node.js 22 or newer.")
        run = subprocess.run([node, str(Path(__file__).with_name("cookiecloud-extension.mjs"))],
                             input=json.dumps(payload), text=True, stdout=subprocess.PIPE,
                             stderr=subprocess.DEVNULL, timeout=timeout, check=False)
        result = json.loads(run.stdout)
    except (OSError, ValueError, subprocess.TimeoutExpired):
        raise SkillError("COOKIECLOUD_CHROME_UNAVAILABLE", "Chrome configuration could not be confirmed. Check the authorized debugging connection and extension before retrying.") from None
    if run.returncode or not isinstance(result, dict) or result.get("ok") is not True:
        stage = result.get("stage") if isinstance(result, dict) else None
        raise SkillError("COOKIECLOUD_CHROME_UNAVAILABLE", "Chrome configuration could not be confirmed. Check the authorized debugging connection and installed CookieCloud extension.", stage=stage if stage in {"connect", "extension", "read", "write", "reload", "upload"} else None)
    return result


def _timestamp(value):
    if not isinstance(value, str):
        return None
    try:
        result = datetime.fromisoformat(value.replace("Z", "+00:00"))
        return result if result.tzinfo is not None else None
    except ValueError:
        return None


def synchronize(args, api):
    validate_cdp_url(args.cdp_url)
    selected = domains(args.domain)
    if args.timeout_seconds < 5:
        raise SkillError("VALIDATION_ERROR", "Allow at least five seconds for Chrome configuration.")
    path = "/v1/desktops/" + quote(args.desktop_id, safe="") + "/browser-sync/config"
    try:
        data = _call(api, "GET", path)
    except SkillError as exc:
        if exc.code != "cookiecloud_not_connected":
            raise
        try:
            data = _call(api, "POST", path, {"domains": selected})
        except SkillError as failure:
            if failure.code not in RECOVERABLE_CREATE:
                raise
            try:
                data = _call(api, "GET", path)
            except SkillError:
                raise failure from None
    settings, allowed = _configuration(data, args.desktop_id)
    # Upload groups use exact configured keywords. Cookie hostnames below those
    # keywords are filtered by the receiver; a new keyword is a scope change.
    if not set(selected) <= set(allowed):
        raise SkillError("COOKIECLOUD_DOMAIN_SCOPE", "Requested domains are not selected in the existing CUA connection. Confirm the intended scope in CUA before changing it.", domains=selected, connection_domains=allowed)
    settings["domains"] = "\n".join(selected)
    before = _timestamp(data.get("received_at"))
    result = _configure_extension({"cdp_url": args.cdp_url, "extension_id": EXTENSION_ID,
                                   "storage_key": STORAGE_KEY, "settings": settings,
                                   "confirm_domains": args.confirm_domains}, args.timeout_seconds)
    if result.get("confirmation_required") is True:
        return {"data": {"status": "user_action_required", "configured": False,
                         "domains": selected, "local_domains": result.get("local_domains", []),
                         "local_scope_all_websites": result.get("local_scope_all_websites") is True,
                         "local_scope_unrecognized": result.get("local_scope_unrecognized") is True},
                "next": {"agent_hint": "Explain the local and requested website lists and ask the user to confirm replacement. Only after confirmation, repeat with --confirm-domains."}}
    configured = result.get("configured") is True and result.get("readback_verified") is True
    if not configured:
        raise SkillError("COOKIECLOUD_CHROME_UNAVAILABLE", "Extension configuration readback was not confirmed.")
    # The extension's test completes its upload request before returning. Read
    # the authoritative receipt once; never manufacture an upload or replay it.
    after = None
    uploaded = False
    try:
        after_data = _call(api, "GET", path)
        after_settings, _ = _configuration(after_data, args.desktop_id)
        after = _timestamp(after_data.get("received_at"))
        uploaded = result.get("upload_attempted") is True and after_settings["uuid"] == settings["uuid"] and after is not None and (before is None or after > before)
    except SkillError:
        # Writes have already been verified. An unavailable receipt is a
        # partial result, not a transient error inviting a second upload.
        pass
    return {"data": {"status": "uploaded" if uploaded else "configured",
                     "desktop_id": args.desktop_id, "domains": selected,
                     "configured": True, "readback_verified": True, "uploaded": uploaded,
                     "received_at": after.isoformat() if after else None},
            "next": {"agent_hint": "Upload receipt is confirmed. Refresh the selected remote websites and verify login. If the desktop does not receive updates automatically, use Import selected websites in CUA." if uploaded else
                     "Configuration is saved, but a new upload receipt is unconfirmed. Check CookieCloud and CUA upload status; do not claim upload or website login success or retry automatically."}}
