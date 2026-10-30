#!/usr/bin/env python3
"""Extract the brand icons of three model vendors from @lobehub/icons-static-svg into inline paths.

Output: `web/src/components/modelIconPaths.ts`, used by `ModelIcon.tsx` to draw the vendor
logo in the model picker. **The output is generated; do not edit it by hand**. Re-running this
script overwrites it.

How to run (from the repo root):

    python3 scripts/model-icons/generate.py

Idempotent: re-running against the same upstream version produces no diff.

# Why inline instead of adding a dependency

We only use 3 icons. `@lobehub/icons` (the React component version) pulls in three peer
dependencies, antd 6 + @lobehub/ui + antd-style, 9 MB, which clash directly with this repo's
HeroUI 3 + Tailwind 4. `@lobehub/icons-static-svg` is clean (plain SVG, zero peer
dependencies), but we only need three files out of 2.3 MB, and importing SVG as an asset in
Vite needs an extra plugin (this repo has none today), while `<img src>` would make the icons
go over HTTP.

The repo already has a precedent and reason for inlining: `scripts/brand-icons/generate.py` +
`web/src/components/brandPaths.ts` (the brand slot is above the fold, and `<img>` would arrive
one RTT later). The "icons must work offline" rule behind `web/src/lib/icons.ts` points the
same way.

# Upstream and license

Source: https://github.com/lobehub/lobe-icons, MIT. We take the `*-color.svg` variant (the
brand's own colors, not the monochrome outline variant). **The icons themselves are the
trademarks of their companies**; here they are only used to identify vendors inside the
product.
"""

import json
import re
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent.parent
OUT = REPO / "web/src/components/modelIconPaths.ts"

PKG = "@lobehub/icons-static-svg"

# vendor key -> upstream file name.
#
# Keys must match the `vendor` field in MODEL_SPECS exactly (web/src/types/domain.ts);
# ModelIcon looks them up by that. Upstream also has chatglm / glmv, and **we do not use them**:
# the three Zhipu models are named glm-* on Ark, but the brand owner is Zhipu, so zhipu is the
# vendor logo. Also, chatglm-color.svg has a <defs> gradient with a hard-coded gradient id;
# showing it twice on one page makes the ids collide (the later one overrides the earlier
# definition), so inlining it safely would need extra id rewriting.
VENDORS = {
    "豆包": "doubao-color",
    "DeepSeek": "deepseek-color",
    "智谱": "zhipu-color",
}


def fetch() -> Path:
    """Download the upstream package into a temp dir, unpack it, and return the icons/ dir."""
    tmp = Path(tempfile.mkdtemp(prefix="model-icons-"))
    print(f"下载 {PKG} …")
    out = subprocess.run(
        ["npm", "pack", PKG, "--pack-destination", str(tmp)],
        capture_output=True,
        text=True,
        timeout=300,
    )
    if out.returncode != 0:
        sys.exit(f"npm pack 失败：{out.stderr.strip()}")
    tgz = next(tmp.glob("*.tgz"), None)
    if tgz is None:
        sys.exit("npm pack 没产出 .tgz")
    with tarfile.open(tgz) as tf:
        tf.extractall(tmp)  # noqa: S202 — upstream is the official package on the npm registry
    return tmp / "package"


def version_of(pkg_root: Path) -> str:
    return json.loads((pkg_root / "package.json").read_text())["version"]


def parse(svg: str, name: str) -> list[dict]:
    """Extract a list of (d, fill) from one SVG.

    Only the plainest shape is accepted: `<path d fill>`, no <defs>, no gradients, no transform.
    The three upstream files happen to all be like that. **If one is not, exit with an error**:
    skipping silently would produce an icon missing half its drawing, and that defect is hard to
    see at 16px.
    """
    for bad, why in (
        ("<defs", "带 <defs>（渐变/滤镜），内联要改写 id 防撞车"),
        ("linearGradient", "带渐变"),
        ("url(#", "引用了 defs 里的 id"),
        ("transform=", "带 transform，抽路径会丢变换"),
        ("<g ", "有分组，可能带组级样式"),
    ):
        if bad in svg:
            sys.exit(f"{name}: {why}——本脚本只处理朴素 path，请人工确认后再改脚本")

    vb = re.search(r'viewBox="([^"]+)"', svg)
    if not vb or vb.group(1).strip() != "0 0 24 24":
        sys.exit(f"{name}: viewBox 不是 0 0 24 24（实际 {vb and vb.group(1)}）")

    paths = []
    for m in re.finditer(r"<path\b([^>]*?)/?>", svg):
        attrs = m.group(1)
        d = re.search(r'\sd="([^"]+)"', attrs)
        if not d:
            sys.exit(f"{name}: 有个 <path> 没有 d 属性")
        fill = re.search(r'\sfill="([^"]+)"', attrs)
        paths.append({"d": d.group(1), "fill": fill.group(1) if fill else None})

    if not paths:
        sys.exit(f"{name}: 一条 path 都没抽到")
    return paths


def main() -> None:
    root = fetch()
    version = version_of(root)
    print(f"上游版本 {version}")

    data: dict[str, list[dict]] = {}
    for vendor, fname in VENDORS.items():
        f = root / "icons" / f"{fname}.svg"
        if not f.exists():
            sys.exit(f"上游没有 {fname}.svg —— 上游改了命名？")
        paths = parse(f.read_text(), fname)
        data[vendor] = paths
        fills = ", ".join(p["fill"] or "currentColor" for p in paths)
        print(f"  {vendor:<10} {fname:<16} {len(paths)} 段  [{fills}]")

    OUT.write_text(render(data, version))
    print(f"\n已写 {OUT.relative_to(REPO)}")


def render(data: dict[str, list[dict]], version: str) -> str:
    lines = [
        "/**",
        " * Brand icon paths for model vendors (Doubao / DeepSeek / Zhipu).",
        " *",
        " * **This file is generated; do not edit it by hand.** scripts/model-icons/generate.py extracts it from",
        f" * @lobehub/icons-static-svg@{version}, and re-running the script overwrites it.",
        " * Upstream is https://github.com/lobehub/lobe-icons (MIT); the icons themselves are each company's trademark,",
        " * used here only to identify vendors inside the product. To add a vendor, add a line to VENDORS in the script and re-run it.",
        " *",
        " * Coordinates are on a 0 0 24 24 canvas, same as upstream. We take the brand-color versions (*-color),",
        " * so fill is a hard-coded hex and **does not follow the theme**: a vendor mark should keep its own colors,",
        " * the same in light and dark themes (all three colors are medium-high saturation, visible enough on both page backgrounds).",
        " *",
        " * Keys are the `vendor` field in MODEL_SPECS; the lookup is in ModelIcon.tsx.",
        " */",
        "",
        "/** Canvas side length. All path coordinates are within 0..24. */",
        "export const MODEL_ICON_VIEWBOX = 24;",
        "",
        "/** One path: `d` and its own fill color. */",
        "export interface ModelIconPath {",
        "  d: string;",
        "  fill: string;",
        "}",
        "",
        "/** vendor → the path list of that vendor's icon, in drawing order. */",
        "export const MODEL_ICON_PATHS: Record<string, ModelIconPath[]> = {",
    ]
    for vendor, paths in data.items():
        lines.append(f'  "{vendor}": [')
        for p in paths:
            fill = p["fill"] or "currentColor"
            lines.append("    {")
            lines.append(f'      d: "{p["d"]}",')
            lines.append(f'      fill: "{fill}",')
            lines.append("    },")
        lines.append("  ],")
    lines.append("};")
    lines.append("")
    return "\n".join(lines)


if __name__ == "__main__":
    main()
