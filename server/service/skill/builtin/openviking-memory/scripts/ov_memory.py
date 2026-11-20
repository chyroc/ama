#!/usr/bin/env python3
"""OpenViking Memory & Knowledge CLI for AMA Sandbox Containers.

Provides active memory and knowledge operations backed by Volcengine VikingDB OpenViking:
- search / find       : Semantic retrieval (L0/L1/L2) across memories, resources, and skills
- read                : Progressive reading (abstract / overview / full) of viking:// URIs
- list / ls / tree    : Browse the viking:// virtual filesystem
- grep / glob         : Keyword regex search and filename pattern matching
- remember            : Extract and persist long-term memories from facts/preferences/decisions
- write / edit        : Create, overwrite, append, or surgically edit a viking:// file
- forget              : Permanently remove an outdated or incorrect viking:// URI
- add-resource        : Ingest an external URL or local file into viking://resources/
- status              : Verify connection and database status
"""

from __future__ import annotations

import argparse
import json
import os
import pathlib
import sys
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Dict, List, Optional


def _load_default_url() -> str:
    cfg_path = pathlib.Path(__file__).resolve().parent.parent / "assets" / "config.json"
    if cfg_path.is_file():
        try:
            data = json.loads(cfg_path.read_text(encoding="utf-8"))
            url = str(data.get("default_url", "")).strip()
            if url:
                return url.rstrip("/")
        except Exception:
            pass
    return "https://api.vikingdb.cn-beijing.volces.com/openviking"


class OpenVikingClient:
    def __init__(
        self,
        base_url: Optional[str] = None,
        api_key: Optional[str] = None,
        resource_id: Optional[str] = None,
        session_id: Optional[str] = None,
        timeout: float = 30.0,
    ) -> None:
        raw_url = (
            base_url
            or os.environ.get("OPENVIKING_URL")
            or os.environ.get("AMA_OPENVIKING_URL")
            or _load_default_url()
        ).strip()
        if raw_url.endswith("/mcp"):
            raw_url = raw_url[:-4]
        self.base_url = raw_url.rstrip("/")
        self.api_key = (
            api_key
            or os.environ.get("OPENVIKING_API_KEY")
            or os.environ.get("AMA_OPENVIKING_API_KEY")
            or ""
        ).strip()
        self.resource_id = (
            resource_id
            or os.environ.get("OPENVIKING_RESOURCE_ID")
            or os.environ.get("AMA_OPENVIKING_RESOURCE_ID")
            or ""
        ).strip()
        self.session_id = (
            session_id
            or os.environ.get("OPENVIKING_SESSION_ID")
            or os.environ.get("AMA_SESSION_ID")
            or ""
        ).strip()
        self.timeout = timeout
        self._mcp_session_id: Optional[str] = None

        if not self.api_key:
            raise RuntimeError(
                "未检测到 OPENVIKING_API_KEY 环境变量，请确认平台或 Agent 已配置 OpenViking API Key。"
            )

    def _headers(self, extra: Optional[Dict[str, str]] = None) -> Dict[str, str]:
        h: Dict[str, str] = {
            "Authorization": f"Bearer {self.api_key}",
            "X-API-Key": self.api_key,
            "Content-Type": "application/json",
            "Accept": "application/json, text/event-stream",
        }
        if self.resource_id:
            h["X-OpenViking-Resource"] = self.resource_id
        if extra:
            h.update(extra)
        return h

    def rest(
        self,
        method: str,
        path: str,
        body: Optional[Dict[str, Any]] = None,
        query: Optional[Dict[str, Any]] = None,
    ) -> Dict[str, Any]:
        url = f"{self.base_url}{path}"
        if query:
            clean_q = {k: str(v) for k, v in query.items() if v is not None and v != ""}
            if clean_q:
                sep = "&" if "?" in url else "?"
                url = f"{url}{sep}{urllib.parse.urlencode(clean_q)}"
        data = json.dumps(body).encode("utf-8") if body is not None else None
        req = urllib.request.Request(url, data=data, headers=self._headers(), method=method)
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                raw = resp.read().decode("utf-8", errors="replace")
                return json.loads(raw) if raw.strip() else {"status": "ok"}
        except urllib.error.HTTPError as e:
            err_body = e.read().decode("utf-8", errors="replace")
            raise RuntimeError(f"HTTP {e.code} on {method} {path}: {err_body}") from e

    def _ensure_mcp_session(self) -> str:
        if self._mcp_session_id:
            return self._mcp_session_id
        url = f"{self.base_url}/mcp"
        payload = {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "initialize",
            "params": {
                "protocolVersion": "2024-11-05",
                "capabilities": {},
                "clientInfo": {"name": "ama-openviking-skill", "version": "1.0.0"},
            },
        }
        req = urllib.request.Request(
            url,
            data=json.dumps(payload).encode("utf-8"),
            headers=self._headers(),
            method="POST",
        )
        with urllib.request.urlopen(req, timeout=self.timeout) as resp:
            sid = resp.headers.get("mcp-session-id") or ""
            self._mcp_session_id = sid
            return sid

    def mcp_call(self, tool_name: str, arguments: Dict[str, Any]) -> str:
        sid = self._ensure_mcp_session()
        url = f"{self.base_url}/mcp"
        clean_args = {k: v for k, v in arguments.items() if v is not None}
        payload = {
            "jsonrpc": "2.0",
            "id": 2,
            "method": "tools/call",
            "params": {"name": tool_name, "arguments": clean_args},
        }
        extra = {"mcp-session-id": sid} if sid else {}
        req = urllib.request.Request(
            url,
            data=json.dumps(payload).encode("utf-8"),
            headers=self._headers(extra),
            method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                raw = resp.read().decode("utf-8", errors="replace").strip()
        except urllib.error.HTTPError as e:
            err_body = e.read().decode("utf-8", errors="replace")
            raise RuntimeError(f"MCP tool '{tool_name}' HTTP {e.code}: {err_body}") from e

        # Handle either JSON or SSE event stream response
        if raw.startswith("event:") or "\ndata:" in raw:
            lines = [
                line[5:].strip()
                for line in raw.splitlines()
                if line.startswith("data:") and line[5:].strip()
            ]
            if lines:
                raw = lines[-1]

        data = json.loads(raw)
        if "error" in data and data["error"]:
            raise RuntimeError(f"MCP error in '{tool_name}': {json.dumps(data['error'], ensure_ascii=False)}")
        result = data.get("result") or {}
        is_error = result.get("isError", False)
        contents = result.get("content") or []
        texts: List[str] = []
        for item in contents:
            if isinstance(item, dict) and item.get("type") == "text":
                texts.append(str(item.get("text", "")))
        joined = "\n".join(texts).strip()
        if is_error:
            raise RuntimeError(joined or f"MCP tool '{tool_name}' failed")
        return joined or json.dumps(result, ensure_ascii=False, indent=2)


def cmd_status(client: OpenVikingClient, _args: argparse.Namespace) -> None:
    res = client.rest("GET", "/api/v1/system/status")
    out = {
        "ok": res.get("status") == "ok",
        "base_url": client.base_url,
        "resource_id": client.resource_id or "(bound to API key)",
        "session_id": client.session_id or "(none)",
        "server_status": res.get("result"),
    }
    print(json.dumps(out, ensure_ascii=False, indent=2))


def _normalize_uri(uri: str) -> str:
    u = (uri or "").strip()
    if u == "viking://user/memories" or u.startswith("viking://user/memories/"):
        return "viking://~/memories" + u[len("viking://user/memories"):]
    if u == "viking://agent/memories" or u.startswith("viking://agent/memories/"):
        return "viking://agent/default/memories" + u[len("viking://agent/memories"):]
    return u


def cmd_search(client: OpenVikingClient, args: argparse.Namespace) -> None:
    query = (args.query_opt or args.query or "").strip()
    if not query:
        raise RuntimeError("search 必须提供检索词（位置参数或 --query）")
    target_uri = _normalize_uri(args.target_uri or args.uri_opt or "")
    params: Dict[str, Any] = {
        "query": query,
        "mode": args.mode,
        "limit": args.limit,
    }
    if args.mode == "context":
        params["max_tokens"] = args.max_tokens
        params["purpose"] = args.purpose
        if client.session_id:
            params["session_id"] = client.session_id
    else:
        if target_uri:
            params["target_uri"] = target_uri
        if args.read_content:
            params["read_content"] = True
    if args.context_type:
        params["context_type"] = [args.context_type]
    if args.min_score is not None:
        params["min_score"] = args.min_score

    print(client.mcp_call("search", params))


def cmd_find(client: OpenVikingClient, args: argparse.Namespace) -> None:
    query = (args.query_opt or args.query or "").strip()
    if not query:
        raise RuntimeError("find 必须提供检索词（位置参数或 --query）")
    target_uri = _normalize_uri(args.target_uri or args.uri_opt or "")
    params: Dict[str, Any] = {
        "query": query,
        "limit": args.limit,
    }
    if target_uri:
        params["target_uri"] = target_uri
    if args.context_type:
        params["context_type"] = [args.context_type]
    if args.min_score is not None:
        params["min_score"] = args.min_score
    if args.read_content:
        params["read_content"] = True
    print(client.mcp_call("find", params))


def cmd_read(client: OpenVikingClient, args: argparse.Namespace) -> None:
    uris: List[str] = []
    if args.uri_opt:
        uris.append(_normalize_uri(args.uri_opt))
    if args.uris:
        uris.extend(_normalize_uri(u) for u in args.uris)
    if not uris:
        raise RuntimeError("read 必须提供至少一个 viking:// URI（位置参数或 --uri）")
    if args.level in ("abstract", "overview"):
        endpoint = "/api/v1/content/abstract" if args.level == "abstract" else "/api/v1/content/overview"
        outputs: List[str] = []
        for uri in uris:
            res = client.rest("GET", endpoint, query={"uri": uri})
            val = res.get("result")
            text = val if isinstance(val, str) else json.dumps(val, ensure_ascii=False, indent=2)
            outputs.append(f"=== {uri} ({args.level}) ===\n{text}")
        print("\n\n".join(outputs))
        return

    params: Dict[str, Any] = {
        "uris": uris,
        "offset": args.offset,
        "limit": args.limit,
    }
    print(client.mcp_call("read", params))


def cmd_list(client: OpenVikingClient, args: argparse.Namespace) -> None:
    uri = _normalize_uri(args.uri_opt or args.uri or "viking://")
    params: Dict[str, Any] = {
        "uri": uri,
        "recursive": args.recursive,
        "limit": args.limit,
    }
    print(client.mcp_call("list", params))


def cmd_tree(client: OpenVikingClient, args: argparse.Namespace) -> None:
    uri = _normalize_uri(args.uri_opt or args.uri or "viking://")
    params: Dict[str, Any] = {
        "uri": uri,
        "level_limit": args.depth,
        "include_abstract": args.include_abstract,
    }
    print(client.mcp_call("tree", params))


def cmd_grep(client: OpenVikingClient, args: argparse.Namespace) -> None:
    pattern = (args.pattern_opt or args.pattern or "").strip()
    if not pattern:
        raise RuntimeError("grep 必须提供搜索模式（位置参数或 --pattern）")
    params: Dict[str, Any] = {
        "uri": _normalize_uri(args.uri),
        "pattern": pattern,
        "case_insensitive": args.ignore_case,
    }
    print(client.mcp_call("grep", params))


def cmd_glob(client: OpenVikingClient, args: argparse.Namespace) -> None:
    pattern = (args.pattern_opt or args.pattern or "").strip()
    if not pattern:
        raise RuntimeError("glob 必须提供通配符模式（位置参数或 --pattern）")
    params: Dict[str, Any] = {
        "uri": _normalize_uri(args.uri),
        "pattern": pattern,
    }
    print(client.mcp_call("glob", params))


def cmd_remember(client: OpenVikingClient, args: argparse.Namespace) -> None:
    parts: List[str] = []
    if args.content_opt:
        parts.append(args.content_opt)
    if args.content:
        parts.extend(args.content)
    content = " ".join(parts).strip()
    if not content:
        raise RuntimeError("remember 内容不能为空（可传位置参数或 --content）")
    messages = [{"role": args.role, "content": content}]
    params: Dict[str, Any] = {"messages": messages}
    if client.session_id:
        params["session_id"] = client.session_id
        env_id = (os.environ.get("ARK_ENVIRONMENT_ID") or "").strip()
        vault_ids_raw = (os.environ.get("ARK_VAULT_IDS") or "").strip()
        meta: Dict[str, Any] = {}
        if env_id:
            meta["env_id"] = env_id
        if vault_ids_raw:
            meta["vault_ids"] = vault_ids_raw
        req_body: Dict[str, Any] = {"session_id": client.session_id}
        if meta:
            req_body["metadata"] = meta
        try:
            client.rest("POST", "/api/v1/sessions", body=req_body)
        except Exception:
            pass
    out = client.mcp_call("remember", params)
    print(out)


def cmd_write(client: OpenVikingClient, args: argparse.Namespace) -> None:
    uri = (args.uri_opt or args.uri or "").strip()
    if not uri:
        raise RuntimeError("write 必须提供目标 viking:// URI（位置参数或 --uri）")
    content = args.content_opt if args.content_opt is not None else args.content
    if args.file:
        content = pathlib.Path(args.file).read_text(encoding="utf-8")
    if content is None:
        raise RuntimeError("必须提供写入内容（位置参数、--content 或 --file）")
    params: Dict[str, Any] = {
        "uri": uri,
        "content": content,
        "mode": args.mode,
        "wait": args.wait,
    }
    print(client.mcp_call("write", params))


def cmd_edit(client: OpenVikingClient, args: argparse.Namespace) -> None:
    uri = (args.uri_opt or args.uri or "").strip()
    if not uri:
        raise RuntimeError("edit 必须提供目标 viking:// URI（位置参数或 --uri）")
    params: Dict[str, Any] = {
        "uri": uri,
        "old_string": args.old_string,
        "new_string": args.new_string,
        "replace_all": args.replace_all,
        "wait": args.wait,
    }
    print(client.mcp_call("edit", params))


def cmd_forget(client: OpenVikingClient, args: argparse.Namespace) -> None:
    target = (args.uri_opt or args.query_opt or args.uri or "").strip()
    if not target:
        raise RuntimeError("forget 必须提供要删除的 viking:// URI（--uri）或搜索词（--query）")
    if target.startswith("viking://") and not args.query_opt:
        params: Dict[str, Any] = {
            "uri": target,
            "recursive": args.recursive,
        }
        print(client.mcp_call("forget", params))
        return

    # Search-based forget (matches codex-memory-plugin's memory_forget behavior)
    res = client.rest("POST", "/api/v1/search/find", body={
        "query": target,
        "target_uri": "viking://user",
        "limit": 5,
    })
    memories = (res.get("result") or {}).get("memories") or []
    candidates = [
        m for m in memories
        if isinstance(m, dict)
        and str(m.get("uri", "")).startswith("viking://")
        and not str(m.get("uri", "")).endswith((".abstract.md", ".overview.md"))
    ]
    if not candidates:
        print(json.dumps({
            "status": "not_found",
            "query": target,
            "message": "未找到匹配的记忆条目",
        }, ensure_ascii=False, indent=2))
        return

    top = candidates[0]
    top_uri = str(top.get("uri"))
    out = client.mcp_call("forget", {"uri": top_uri, "recursive": False})
    print(json.dumps({
        "status": "deleted",
        "deleted_uri": top_uri,
        "score": top.get("score"),
        "abstract": top.get("abstract"),
        "detail": out,
    }, ensure_ascii=False, indent=2))


def cmd_add_resource(client: OpenVikingClient, args: argparse.Namespace) -> None:
    params: Dict[str, Any] = {
        "path": args.path,
    }
    if args.to:
        params["to"] = args.to
    if args.description:
        params["description"] = args.description
    print(client.mcp_call("add_resource", params))


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="ov_memory.py",
        description="OpenViking 长期记忆与上下文知识库 CLI 工具",
    )
    p.add_argument("--url", help="覆盖 OPENVIKING_URL")
    p.add_argument("--api-key", help="覆盖 OPENVIKING_API_KEY")
    p.add_argument("--resource-id", help="覆盖 OPENVIKING_RESOURCE_ID")
    p.add_argument("--session-id", help="覆盖 OPENVIKING_SESSION_ID")
    p.add_argument("--timeout", type=float, default=30.0, help="HTTP 超时秒数 (默认 30)")

    sub = p.add_subparsers(dest="subcommand", required=True)

    # status
    s_status = sub.add_parser("status", help="检查 OpenViking 连接与状态")
    s_status.set_defaults(func=cmd_status)

    # search
    s_search = sub.add_parser("search", help="深度语义检索（支持 list 与 context 两种模式）")
    s_search.add_argument("query", nargs="?", default="", help="自然语言检索词")
    s_search.add_argument("--query", "-q", dest="query_opt", default="", help="自然语言检索词")
    s_search.add_argument("--mode", choices=["list", "context"], default="list", help="list 返回条目列表；context 返回格式化上下文块")
    s_search.add_argument("--target-uri", default="", help="限定检索范围 URI（如 viking://user 或 viking://resources）")
    s_search.add_argument("--uri", dest="uri_opt", default="", help="--target-uri 的别名")
    s_search.add_argument("--context-type", choices=["memory", "resource", "skill"], help="过滤结果类型")
    s_search.add_argument("--limit", type=int, default=10, help="最大返回条数 (默认 10)")
    s_search.add_argument("--min-score", type=float, default=None, help="最低相关度分数阈值")
    s_search.add_argument("--max-tokens", type=int, default=1500, help="mode=context 时的最大 Token 预算")
    s_search.add_argument("--purpose", choices=["chat", "coding"], default="chat", help="mode=context 时的场景预设")
    s_search.add_argument("--read-content", action="store_true", help="同时内联返回匹配文件正文")
    s_search.set_defaults(func=cmd_search)

    # find
    s_find = sub.add_parser("find", help="快速语义向量查找（不带会话改写）")
    s_find.add_argument("query", nargs="?", default="", help="检索词")
    s_find.add_argument("--query", "-q", dest="query_opt", default="", help="检索词")
    s_find.add_argument("--target-uri", default="", help="限定检索范围 URI")
    s_find.add_argument("--uri", dest="uri_opt", default="", help="--target-uri 的别名")
    s_find.add_argument("--context-type", choices=["memory", "resource", "skill"], help="过滤结果类型")
    s_find.add_argument("--limit", type=int, default=10, help="最大返回条数 (默认 10)")
    s_find.add_argument("--min-score", type=float, default=None, help="最低相关度分数阈值")
    s_find.add_argument("--read-content", action="store_true", help="同时内联返回匹配文件正文")
    s_find.set_defaults(func=cmd_find)

    # read
    s_read = sub.add_parser("read", help="读取一个或多个 viking:// URI 的内容（支持 abstract/overview/full）")
    s_read.add_argument("uris", nargs="*", help="要读取的 viking:// URI")
    s_read.add_argument("--uri", dest="uri_opt", default="", help="要读取的单个 viking:// URI")
    s_read.add_argument("--level", choices=["full", "overview", "abstract"], default="full", help="读取层级：full(L2全文,默认) / overview(L1概览) / abstract(L0摘要)")
    s_read.add_argument("--offset", type=int, default=0, help="起始行号 (默认 0)")
    s_read.add_argument("--limit", type=int, default=2000, help="最大读取行数 (默认 2000)")
    s_read.set_defaults(func=cmd_read)

    # list / ls
    for name in ("list", "ls"):
        s_list = sub.add_parser(name, help="列出 viking:// 目录下的条目")
        s_list.add_argument("uri", nargs="?", default="viking://", help="目录 URI (默认 viking://)")
        s_list.add_argument("--uri", dest="uri_opt", default="", help="目录 URI")
        s_list.add_argument("-r", "--recursive", action="store_true", help="递归列出子目录")
        s_list.add_argument("--limit", type=int, default=256, help="最大返回条目数")
        s_list.set_defaults(func=cmd_list)

    # tree
    s_tree = sub.add_parser("tree", help="以树形结构展示 viking:// 目录")
    s_tree.add_argument("uri", nargs="?", default="viking://", help="目录 URI (默认 viking://)")
    s_tree.add_argument("--uri", dest="uri_opt", default="", help="目录 URI")
    s_tree.add_argument("--depth", type=int, default=3, help="最大遍历深度 (默认 3)")
    s_tree.add_argument("--include-abstract", action="store_true", help="同时显示每个条目的摘要")
    s_tree.set_defaults(func=cmd_tree)

    # grep
    s_grep = sub.add_parser("grep", help="在 viking:// 文件中按关键词/正则搜索内容")
    s_grep.add_argument("pattern", nargs="?", default="", help="搜索词或正则表达式")
    s_grep.add_argument("--pattern", "-p", dest="pattern_opt", default="", help="搜索词或正则表达式")
    s_grep.add_argument("--uri", default="viking://", help="起始搜索目录 URI (默认 viking://)")
    s_grep.add_argument("-i", "--ignore-case", action="store_true", help="忽略大小写")
    s_grep.set_defaults(func=cmd_grep)

    # glob
    s_glob = sub.add_parser("glob", help="在 viking:// 中按文件名通配符匹配文件")
    s_glob.add_argument("pattern", nargs="?", default="", help="文件名 glob 模式（如 **/*.md）")
    s_glob.add_argument("--pattern", "-p", dest="pattern_opt", default="", help="文件名 glob 模式")
    s_glob.add_argument("--uri", default="viking://", help="起始目录 URI (默认 viking://)")
    s_glob.set_defaults(func=cmd_glob)

    # remember
    s_rem = sub.add_parser("remember", help="将重要事实、用户偏好或决策主动存入 OpenViking 长期记忆")
    s_rem.add_argument("content", nargs="*", help="要记住的事实、偏好或约定内容")
    s_rem.add_argument("--content", "-c", dest="content_opt", default="", help="要记住的事实、偏好或约定内容")
    s_rem.add_argument("--role", choices=["user", "assistant"], default="user", help="消息角色 (默认 user)")
    s_rem.set_defaults(func=cmd_remember)

    # write
    s_write = sub.add_parser("write", help="直接向指定 viking:// 文件写入文本并建立语义索引")
    s_write.add_argument("uri", nargs="?", default="", help="目标 viking:// URI（如 viking://user/default/memories/preferences/style.md）")
    s_write.add_argument("content", nargs="?", default=None, help="要写入的文本内容")
    s_write.add_argument("--uri", dest="uri_opt", default="", help="目标 viking:// URI")
    s_write.add_argument("--content", "-c", dest="content_opt", default=None, help="要写入的文本内容")
    s_write.add_argument("--file", help="从本地文件读取内容写入")
    s_write.add_argument("--mode", choices=["replace", "append", "create"], default="replace", help="写入模式 (默认 replace)")
    s_write.add_argument("--wait", action="store_true", default=True, help="等待语义索引更新完成 (默认开启)")
    s_write.set_defaults(func=cmd_write)

    # edit
    s_edit = sub.add_parser("edit", help="精确替换指定 viking:// 文件中的局部文本")
    s_edit.add_argument("uri", nargs="?", default="", help="目标 viking:// URI")
    s_edit.add_argument("--uri", dest="uri_opt", default="", help="目标 viking:// URI")
    s_edit.add_argument("--old", dest="old_string", required=True, help="原文件中需要替换的精确文本")
    s_edit.add_argument("--new", dest="new_string", required=True, help="替换后的新文本")
    s_edit.add_argument("--replace-all", action="store_true", help="替换所有匹配项")
    s_edit.add_argument("--wait", action="store_true", default=True, help="等待语义索引更新完成")
    s_edit.set_defaults(func=cmd_edit)

    # forget
    s_forget = sub.add_parser("forget", help="永久删除指定的 viking:// 记忆或按关键词检索删除")
    s_forget.add_argument("uri", nargs="?", default="", help="要删除的 viking:// URI 或搜索词")
    s_forget.add_argument("--uri", dest="uri_opt", default="", help="要删除的 viking:// URI")
    s_forget.add_argument("--query", "-q", dest="query_opt", default="", help="按关键词语义检索并删除最匹配的记忆")
    s_forget.add_argument("-r", "--recursive", action="store_true", help="递归删除目录")
    s_forget.set_defaults(func=cmd_forget)

    # add-resource
    s_add = sub.add_parser("add-resource", help="将外部 URL 导入到 OpenViking 知识库")
    s_add.add_argument("path", help="外部 URL（http/https/git）或本地文件路径")
    s_add.add_argument("--to", default="", help="目标 URI（如 viking://resources/my_doc）")
    s_add.add_argument("--description", default="", help="资源用途说明")
    s_add.set_defaults(func=cmd_add_resource)

    return p


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()
    try:
        client = OpenVikingClient(
            base_url=args.url,
            api_key=args.api_key,
            resource_id=args.resource_id,
            session_id=args.session_id,
            timeout=args.timeout,
        )
        args.func(client, args)
        return 0
    except Exception as exc:
        print(f"[openviking-memory error] {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
