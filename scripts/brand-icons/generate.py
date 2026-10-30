#!/usr/bin/env python3
"""Turn the brand image ama.png from design into the full set of icons + vector paths the frontend needs.

# Why this script exists instead of a one-off manual export

Design delivered a 1254×1254 bitmap. The frontend needs four things: a favicon (must be vector to
be sharp from 16px to 512px), an apple-touch-icon, PWA pngs in two sizes, and **the three paths
used by the React component for the in-app brand mark**. A manual export has two problems: the
bitmap turns into a blur at 16px, and there is no way to verify that the vector paths "really look
like the original".

So this script does **trace + verify**: it traces the three color blocks into Bézier paths, then
rasterizes the paths back and computes IoU against the original masks, and fails if any is below
the threshold. All three shapes are currently above 0.993.

# The source image has no alpha; the dark purple background is solid pixels

`ama.png` is mode RGB; the dark purple #3C043C is a painted background, not transparency.
**Do not cut it out**: first, it cannot be cut cleanly (the AI-generated image has transition
pixels at the edges, leaving a ragged outline); second, at 16px the favicon relies on the
background color to make the outline readable. So all outputs keep the dark purple background.

# Usage

    python3 scripts/brand-icons/generate.py

Works from anywhere in the repo. Outputs go to web/public/, and the three paths are also written
to web/src/components/brandPaths.ts (the component imports them from there instead of piling
coordinates into the tsx). Re-running is idempotent.
"""

from __future__ import annotations

import json
import math
import subprocess
import sys
from collections import deque
from pathlib import Path

import numpy as np
from PIL import Image, ImageDraw

# ── Source image and sampled colors ───────────────────────────────────────────────
# These four values were sampled from the source image; if the source changes, sample again
# (the script uses nearest-neighbor quantization, so a slightly off color is fine, but a new
# color scheme must be updated here).
BG = "#3C043C"  # dark purple background
CREAM = "#FAF6E8"  # cream
RED = "#FD3C1E"  # orange-red
YELLOW = "#FDC70D"  # bright yellow

PALETTE = [BG, CREAM, RED, YELLOW]
SHAPES = ["cream", "red", "yellow"]

# Number of Bézier sample points. 44 was found by trial: 28 points only reach IoU 0.989, 44 points
# put all three shapes above 0.993, and going up to 72 gets slightly worse (overfits quantization
# noise).
SAMPLES = 44
# Fidelity floor. Below it, tracing went wrong (for example the source image changed, or the
# wrong colors were quantized). Better to fail than to produce a logo that "looks about right".
MIN_IOU = 0.99
# Vector canvas size. 64 instead of 1254: coordinates are short and readable, and 64 divides by 2
# many times, so corners do not land on half pixels when scaled to 16/32.
VIEWBOX = 64


def repo_root() -> Path:
    """Repo root. Use git instead of counting levels up from __file__; the latter silently points to the wrong place when the directory layout changes."""
    out = subprocess.run(
        ["git", "rev-parse", "--show-toplevel"],
        cwd=Path(__file__).parent,
        capture_output=True,
        text=True,
        check=True,
    )
    return Path(out.stdout.strip())


def hex_to_rgb(h: str) -> tuple[int, int, int]:
    return tuple(int(h[i : i + 2], 16) for i in (1, 3, 5))  # type: ignore[return-value]


# ── 1. Quantize to four colors and extract masks for the three color blocks ─────────────
def quantize(src: Path) -> np.ndarray:
    """Map each pixel to the nearest palette color; return an index map (0=bg 1=cream 2=orange-red 3=yellow).

    The source image has anti-aliased transition pixels at edges; nearest-neighbor quantization
    assigns each one to whichever side is closer. That is exactly what we want (the boundary lands
    in the middle of the transition band).
    """
    arr = np.asarray(Image.open(src).convert("RGB")).astype(int)
    cols = np.array([hex_to_rgb(c) for c in PALETTE])
    dist = ((arr[:, :, None, :] - cols[None, None, :, :]) ** 2).sum(-1)
    return dist.argmin(-1)


def largest_component(mask: np.ndarray) -> tuple[np.ndarray, int]:
    """Keep only the largest connected component (4-neighborhood); return (mask, number of dropped pixels).

    Quantization leaves dozens of isolated pixels where color blocks meet (orange-red had 661 in
    practice). If they are not removed, contour tracing may start from a fragment and trace a ring
    a few pixels wide. No error, but the path is completely wrong.
    """
    labels = np.zeros(mask.shape, np.int32)
    cur = 0
    best = (0, 0)
    h, w = mask.shape
    for sy in range(h):
        for sx in np.nonzero(mask[sy])[0]:
            if labels[sy, sx]:
                continue
            cur += 1
            size = 0
            queue = deque([(sy, int(sx))])
            labels[sy, sx] = cur
            while queue:
                y, x = queue.popleft()
                size += 1
                for dy, dx in ((1, 0), (-1, 0), (0, 1), (0, -1)):
                    ny, nx = y + dy, x + dx
                    if 0 <= ny < h and 0 <= nx < w and mask[ny, nx] and not labels[ny, nx]:
                        labels[ny, nx] = cur
                        queue.append((ny, nx))
            if size > best[0]:
                best = (size, cur)
    return labels == best[1], int(mask.sum()) - best[0]


# ── 2. Mask → contour polyline ──────────────────────────────────────────────────────
def trace_contour(mask: np.ndarray) -> list[tuple[int, int]]:
    """Trace the outer contour as a polyline of grid points.

    The approach is **directed edge linking**, not the usual "direction walking" state machine:
    give each exposed edge of every solid pixel a direction (so the solid side is always on the
    right of the travel direction). Then every corner point has exactly one outgoing edge, and
    following the next pointers once around closes the loop.

    The benefit is that each edge's direction can be checked on its own. The direction-walking
    approach depends on a table of "current direction × are the two cells ahead solid"; one wrong
    entry and the walk never returns to the start, and the error is "tracing did not converge",
    which does not point at the wrong entry at all.
    """
    h, w = mask.shape
    padded = np.zeros((h + 2, w + 2), bool)
    padded[1:-1, 1:-1] = mask

    def solid(y: int, x: int) -> bool:
        return bool(padded[y + 1, x + 1])

    edges: dict[tuple[int, int], tuple[int, int]] = {}
    ys, xs = np.nonzero(mask)
    for y, x in zip(ys.tolist(), xs.tolist()):
        # Screen y points down, so "solid on the right" = top edge goes right, right edge goes down, bottom edge goes left, left edge goes up
        if not solid(y - 1, x):
            edges[(x, y)] = (x + 1, y)
        if not solid(y, x + 1):
            edges[(x + 1, y)] = (x + 1, y + 1)
        if not solid(y + 1, x):
            edges[(x + 1, y + 1)] = (x, y + 1)
        if not solid(y, x - 1):
            edges[(x, y + 1)] = (x, y)

    start = min(edges)  # top-most, left-most corner; always on the outer contour (never on a hole boundary)
    pts = [start]
    cur = edges[start]
    while cur != start:
        pts.append(cur)
        cur = edges[cur]
        if len(pts) > len(edges) + 2:
            raise RuntimeError("轮廓没有闭合：掩码可能不是单连通的")
    return pts


def resample(pts: list[tuple[int, int]], n: int) -> np.ndarray:
    """Resample evenly by arc length into n points.

    Points must be taken by **arc length**, not by index: grid points are denser along diagonal
    edges than along straight ones (diagonals are staircases, with more grid points per unit
    length). Sampling by index puts too many control points on straight edges and too few on
    corners, and the fitted corners come out flattened.
    """
    p = np.asarray(pts, float)
    seg = np.hypot(*(np.roll(p, -1, 0) - p).T)
    cum = np.concatenate([[0.0], np.cumsum(seg)])
    out = []
    for t in np.linspace(0, cum[-1], n, endpoint=False):
        i = min(int(np.searchsorted(cum, t, "right")) - 1, len(p) - 1)
        frac = (t - cum[i]) / seg[i] if seg[i] else 0.0
        a, b = p[i], p[(i + 1) % len(p)]
        out.append(a + (b - a) * frac)
    return np.asarray(out)


def to_bezier(points: np.ndarray, tension: float = 1 / 6) -> list[tuple]:
    """Closed Catmull-Rom → cubic Bézier segments.

    Catmull-Rom passes through every sample point by construction, so "the path hugs the original
    contour" is guaranteed structurally, not by good parameter tuning; tension only affects how
    full the corners are. 1/6 is the standard Catmull-Rom→Bézier conversion factor.
    """
    n = len(points)
    segs = []
    for i in range(n):
        p0, p1 = points[(i - 1) % n], points[i]
        p2, p3 = points[(i + 1) % n], points[(i + 2) % n]
        segs.append((p1, p1 + (p2 - p0) * tension, p2 - (p3 - p1) * tension, p2))
    return segs


def flatten(segs: list[tuple], steps: int = 24) -> list[tuple[float, float]]:
    """Bézier → dense polyline, only used to rasterize for verification."""
    poly = []
    for p1, c1, c2, p2 in segs:
        for s in range(steps):
            t = s / steps
            mt = 1 - t
            poly.append(
                tuple(mt**3 * p1 + 3 * mt * mt * t * c1 + 3 * mt * t * t * c2 + t**3 * p2)
            )
    return poly


def iou(segs: list[tuple], mask: np.ndarray) -> float:
    """Rasterize the fitted path back to the original resolution and compute IoU with the original mask.

    This is the script's only correctness check. A path that "looks right" means nothing; it has
    to be measurable.
    """
    img = Image.new("1", mask.shape[::-1], 0)
    ImageDraw.Draw(img).polygon(flatten(segs), fill=1)
    got = np.asarray(img, bool)
    return float((got & mask).sum() / (got | mask).sum())


def svg_path(segs: list[tuple], scale: float) -> str:
    """Bézier segments → SVG path d attribute, coordinates scaled to VIEWBOX with two decimals."""

    def f(v: float) -> str:
        # Two decimals: on a 64 canvas 0.01 is 1/6400, far smaller than one pixel at any display size.
        return f"{v * scale:.2f}".rstrip("0").rstrip(".")

    d = [f"M{f(segs[0][0][0])} {f(segs[0][0][1])}"]
    for _, c1, c2, p2 in segs:
        d.append(f"C{f(c1[0])} {f(c1[1])} {f(c2[0])} {f(c2[1])} {f(p2[0])} {f(p2[1])}")
    d.append("Z")
    return "".join(d)


# ── 3. Outputs ──────────────────────────────────────────────────────────────────────
def build_svg(paths: dict[str, str]) -> str:
    """Build favicon.svg.

    **The background plate is full-bleed with square corners, no rounding.** Measured: the
    outermost ring of pixels in the source is all dark purple (corners too), so design delivered a
    full-bleed square. Rounding it would make a decision design did not make, and it would break
    two things at once: PWA maskable icons must be full-bleed (the platform crops to its own shape;
    rounding first gives double rounded corners), and the same goes for the iOS apple-touch-icon.

    The in-app brand mark does need rounded corners, but that is a **display-layer** concern:
    BrandMark rounds it with the repo's existing `r-chip` class (see the four radius levels in
    docs/guides/styling.md). That way the radius has one source, and we never get two values like
    "6px in CSS, 15px in SVG".
    """
    plate = f'<rect width="{VIEWBOX}" height="{VIEWBOX}" fill="{BG}"/>'
    body = "".join(
        f'<path d="{paths[name]}" fill="{color}"/>'
        for name, color in (("cream", CREAM), ("red", RED), ("yellow", YELLOW))
    )
    return (
        f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {VIEWBOX} {VIEWBOX}">'
        f"<title>AMA</title>{plate}{body}</svg>\n"
    )


def write_pngs(src: Path, out_dir: Path) -> list[str]:
    """Downscale the source image into PNGs of each size.

    **These bitmaps are downscaled with LANCZOS directly from the 1254px original**, not rendered
    from the vector (no cairosvg-like library is installed, and Pillow cannot read SVG). At large
    sizes the difference between bitmap and vector is invisible; only the 16/32px sizes need
    vector, and favicon.svg covers those.
    """
    img = Image.open(src).convert("RGB")
    written = []
    # apple-touch-icon is 180×180 (the standard iOS size); PWA needs 192 and 512.
    for name, size in (
        ("apple-touch-icon.png", 180),
        ("icon-192.png", 192),
        ("icon-512.png", 512),
    ):
        img.resize((size, size), Image.LANCZOS).save(out_dir / name, optimize=True)
        written.append(name)
    return written


def write_ico(src: Path, out_dir: Path) -> None:
    """favicon.ico, with 16/32/48 sizes.

    We still ship an ico as a fallback for "places that do not support SVG favicons": Safari has
    supported SVG favicons only recently, and many aggregators/crawlers only fetch /favicon.ico
    from the root. Pillow's ICO save can write several sizes at once; just pass all sizes.
    """
    img = Image.open(src).convert("RGB")
    # Downscale to 48 first and let Pillow generate each size; sharper than going straight from
    # 1254 to 16 (one LANCZOS step of 78x wipes out detail).
    base = img.resize((48, 48), Image.LANCZOS)
    base.save(out_dir / "favicon.ico", sizes=[(16, 16), (32, 32), (48, 48)])


def write_manifest(out_dir: Path) -> None:
    """PWA manifest.

    **The extension is .json, not the usual .webmanifest**: in production the frontend is served
    by Go's http.FileServer from inside the binary (server/httpd/static.go), and
    `mime.TypeByExtension(".webmanifest")` returns an empty string in Go. Content-Type then
    depends on content sniffing, browsers will likely treat it as text/plain and silently ignore
    the whole manifest. .json is application/json on both paths (vite / Go embed).

    Only icons and theme color: this is not meant to be an installable PWA (no display / scope),
    just so "add to home screen" gets a decent icon.
    """
    manifest = {
        "name": "AMA · Agent 平台",
        "short_name": "AMA",
        "start_url": "/",
        "icons": [
            {"src": "/favicon.svg", "type": "image/svg+xml", "sizes": "any"},
            {"src": "/icon-192.png", "type": "image/png", "sizes": "192x192"},
            {"src": "/icon-512.png", "type": "image/png", "sizes": "512x512"},
        ],
        "theme_color": BG,
        "background_color": BG,
    }
    text = json.dumps(manifest, ensure_ascii=False, indent=2) + "\n"
    (out_dir / "manifest.json").write_text(text, encoding="utf-8")


TS_HEADER = '''/**
 * Vector paths for the AMA brand mark.
 *
 * **This file is generated; do not edit it by hand.** scripts/brand-icons/generate.py traces it from the
 * designer's ama.png, and re-running the script overwrites it. To change the logo: "replace the source image → re-run the script".
 *
 * Coordinates are on a 0 0 {vb} {vb} canvas. The three paths are the cream, orange-red and bright yellow blocks.
 * The dark purple base is drawn by the user of this file: **the source is a full-bleed square (corners are dark purple too); rounding is the display layer's job**,
 * so neither the base nor the rounding is written here; BrandMark rounds it with the repo's existing r-chip.
 *
 * Tracing fidelity (IoU of the paths rasterized back to 1254px against the original image mask):
{fidelity}
 */

'''


def write_ts(paths: dict[str, str], ious: dict[str, float], out: Path) -> None:
    fidelity = "\n".join(f" *   {n:<7} {ious[n]:.4f}" for n in SHAPES)
    body = TS_HEADER.format(vb=VIEWBOX, fidelity=fidelity)
    body += f"/** Canvas side length. All path coordinates are within 0..{VIEWBOX}. */\n"
    body += f"export const BRAND_VIEWBOX = {VIEWBOX};\n\n"
    body += "/** Brand colors. Dark purple is the base; the other three are the color blocks. */\nexport const BRAND_COLORS = {\n"
    for key, val in (("plate", BG), ("cream", CREAM), ("red", RED), ("yellow", YELLOW)):
        body += f'  {key}: "{val}",\n'
    body += "} as const;\n\n"
    body += "/** Paths of the three color blocks, in drawing order (they do not overlap, so order does not change the result). */\n"
    body += "export const BRAND_PATHS = {\n"
    for name in SHAPES:
        body += f'  {name}:\n    "{paths[name]}",\n'
    body += "} as const;\n"
    out.write_text(body, encoding="utf-8")


def main() -> int:
    root = repo_root()
    src = Path.home() / "tmp/ama.png"
    if len(sys.argv) > 1:
        src = Path(sys.argv[1])
    if not src.exists():
        print(f"✗ 源图不存在：{src}", file=sys.stderr)
        print("  用法：generate.py [ama.png 的路径]", file=sys.stderr)
        return 1

    public = root / "web/public"
    public.mkdir(parents=True, exist_ok=True)

    print(f"源图 {src}")
    img = Image.open(src)
    print(f"  {img.size[0]}×{img.size[1]} mode={img.mode}"
          f"{'（无 alpha，深紫底是实心像素，不抠）' if img.mode == 'RGB' else ''}")

    idx = quantize(src)
    scale = VIEWBOX / img.size[0]

    paths: dict[str, str] = {}
    ious: dict[str, float] = {}
    print("\n追踪三个色块：")
    for i, name in enumerate(SHAPES, 1):
        raw = idx == i
        mask, dropped = largest_component(raw)
        contour = trace_contour(mask)
        segs = to_bezier(resample(contour, SAMPLES))
        score = iou(segs, mask)
        paths[name] = svg_path(segs, scale)
        ious[name] = score
        flag = "✓" if score >= MIN_IOU else "✗"
        print(
            f"  {flag} {name:<7} 像素 {int(raw.sum()):>7} 丢碎片 {dropped:>4} "
            f"轮廓点 {len(contour):>5} → {SAMPLES} 段  IoU={score:.5f}"
        )

    bad = {n: v for n, v in ious.items() if v < MIN_IOU}
    if bad:
        print(f"\n✗ 保真度低于 {MIN_IOU}：{bad}", file=sys.stderr)
        print("  追踪失败了，产物没有写出。先确认源图配色与 PALETTE 是否一致。", file=sys.stderr)
        return 1

    print("\n写出产物：")
    # Only one SVG: the plate is full-bleed, so favicon and maskable are the same image (see build_svg's docstring).
    (public / "favicon.svg").write_text(build_svg(paths), encoding="utf-8")
    print(f"  web/public/favicon.svg           {(public / 'favicon.svg').stat().st_size} B")

    write_ico(src, public)
    print(f"  web/public/favicon.ico           {(public / 'favicon.ico').stat().st_size} B（16/32/48）")
    for name in write_pngs(src, public):
        print(f"  web/public/{name:<22} {(public / name).stat().st_size} B")

    write_manifest(public)
    print(f"  web/public/manifest.json         {(public / 'manifest.json').stat().st_size} B")

    ts = root / "web/src/components/brandPaths.ts"
    write_ts(paths, ious, ts)
    print(f"  web/src/components/brandPaths.ts {ts.stat().st_size} B")

    print("\n完成。改 logo 就换源图重跑；产物是幂等的。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
