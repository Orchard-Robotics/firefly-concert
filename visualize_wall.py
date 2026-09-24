"""Render Firefly wall snapshots as PNG pictures. Python 3.11+, standard library only.

Takes the JSON returned by `GET /walls/{id}` ({"version": N, "pixels": [4096 ints]}) and
draws the 64×64 wall, brightest cells in the highest energy, with a caption and colour key.

    python visualize_wall.py example_snapshot.json                 # -> example_snapshot.png
    curl -s http://127.0.0.1:8765/walls/demo | python visualize_wall.py - -o demo.png
    python visualize_wall.py --url http://127.0.0.1:8765 --wall demo -o live.png
    python visualize_wall.py --url http://127.0.0.1:8765 --wall demo --frames 6 --interval 0.5
    python visualize_wall.py a.json b.json c.json -o strip.png     # side-by-side sequence
    python visualize_wall.py --diff before.json after.json -o delta.png

Options: --style leds|cells, --scale N (pixels per cell), --log (compress bright outliers),
--vmax N (fix the colour scale so several pictures are comparable), --crop X,Y,W,H
(zoom into a region, e.g. --crop 0,0,8,8 --scale 40 for the worked example).
"""

import argparse
import json
import math
import struct
import sys
import time
import urllib.request
import zlib

SIZE = 64
BACKGROUND = (12, 10, 20)
UNLIT = (30, 27, 42)
TEXT = (230, 226, 240)
DIM_TEXT = (140, 134, 160)
# Dark violet -> magenta -> orange -> pale yellow: dark cells stay distinguishable from 0.
EMBER = [(0.0, (48, 18, 92)), (0.3, (150, 30, 140)), (0.6, (240, 90, 60)),
         (0.85, (255, 190, 60)), (1.0, (255, 250, 210))]
# Diverging key for --diff: blue for cells that lost energy, amber for cells that gained.
LOSS, GAIN = (80, 150, 255), (255, 170, 40)

FONT = {  # 3×5 bitmap glyphs, one string of three bits per row.
    "0": "111101101101111", "1": "010110010010111", "2": "111001111100111",
    "3": "111001111001111", "4": "101101111001001", "5": "111100111001111",
    "6": "111100111101111", "7": "111001010010010", "8": "111101111101111",
    "9": "111101111001111", "A": "010101111101101", "D": "110101101101110",
    "E": "111100110100111", "F": "111100110100100", "G": "111100101101111",
    "I": "111010010010111", "L": "100100100100111", "M": "101111111101101",
    "N": "110101101101101", "O": "111101101101111", "R": "110101110101101",
    "S": "111100111001111", "T": "111010010010010", "V": "101101101101010",
    "W": "101101111111101", "X": "101101010101101", "B": "110101110101110",
    "C": "111100100100111", "H": "101101111101101", "J": "001001001101111",
    "K": "101101110101101", "P": "111101111100100", "Q": "111101101111001",
    "U": "101101101101111", "Y": "101101010010010", "Z": "111001010100111",
    "+": "000010111010000",
    "-": "000000111000000", "=": "000111000111000", ":": "000010000010000",
    " ": "000000000000000", ".": "000000000000010", "/": "001001010100100",
}


class Canvas:
    def __init__(self, width, height, fill=BACKGROUND):
        self.width, self.height = width, height
        self.rows = [bytearray(bytes(fill) * width) for _ in range(height)]

    def rect(self, x, y, w, h, color):
        for row in range(max(0, y), min(self.height, y + h)):
            lo, hi = max(0, x), min(self.width, x + w)
            if hi > lo:
                self.rows[row][lo * 3:hi * 3] = bytes(color) * (hi - lo)

    def disc(self, cx, cy, radius, color, glow=None):
        """Filled circle with an optional soft halo, blended over the background."""
        reach = radius + (radius if glow else 0)
        for row in range(int(cy - reach), int(cy + reach) + 1):
            if not 0 <= row < self.height:
                continue
            for col in range(int(cx - reach), int(cx + reach) + 1):
                if not 0 <= col < self.width:
                    continue
                d = math.hypot(col + 0.5 - cx, row + 0.5 - cy)
                if d <= radius:
                    alpha = min(1.0, radius - d + 0.5)
                elif glow and d <= reach:
                    alpha = glow * (1 - (d - radius) / radius) ** 2
                else:
                    continue
                i = col * 3
                px = self.rows[row]
                for c in range(3):
                    px[i + c] = round(px[i + c] + (color[c] - px[i + c]) * alpha)

    def text(self, x, y, message, color=TEXT, scale=2):
        for ch in message.upper():
            glyph = FONT.get(ch, FONT[" "])
            for bit, on in enumerate(glyph):
                if on == "1":
                    self.rect(x + (bit % 3) * scale, y + (bit // 3) * scale, scale, scale, color)
            x += 4 * scale

    def paste(self, other, x, y):
        for row in range(other.height):
            self.rows[y + row][x * 3:(x + other.width) * 3] = other.rows[row]

    def png(self):
        raw = b"".join(b"\x00" + bytes(row) for row in self.rows)

        def chunk(kind, data):
            return (struct.pack(">I", len(data)) + kind + data
                    + struct.pack(">I", zlib.crc32(kind + data) & 0xFFFFFFFF))

        header = struct.pack(">IIBBBBB", self.width, self.height, 8, 2, 0, 0, 0)
        return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", header)
                + chunk(b"IDAT", zlib.compress(raw, 9)) + chunk(b"IEND", b""))


def ramp(t, stops=EMBER):
    t = min(1.0, max(0.0, t))
    for (t0, c0), (t1, c1) in zip(stops, stops[1:]):
        if t <= t1:
            f = (t - t0) / (t1 - t0)
            return tuple(round(a + (b - a) * f) for a, b in zip(c0, c1))
    return stops[-1][1]


def validate(snapshot, source):
    if not isinstance(snapshot, dict) or set(snapshot) != {"version", "pixels"}:
        raise SystemExit(f"{source}: expected {{'version', 'pixels'}}, got {type(snapshot).__name__}"
                         f" with keys {sorted(snapshot) if isinstance(snapshot, dict) else '-'}")
    pixels = snapshot["pixels"]
    if not isinstance(pixels, list) or len(pixels) != SIZE * SIZE:
        raise SystemExit(f"{source}: pixels must be a flat list of {SIZE * SIZE} integers")
    if not all(type(p) is int and p >= 0 for p in pixels):
        raise SystemExit(f"{source}: pixels must be nonnegative integers")
    return snapshot


def render(values, args, color_of, caption, subcaption, key):
    """Draw one panel of the crop region; color_of(value) returns RGB, or None for unlit."""
    scale, style = args.scale, args.style
    x0, y0, cols, rows = args.crop
    pad, top, key_h = 16, 56, 30
    panel = Canvas(max(cols * scale + 2 * pad, 300), rows * scale + pad + top + key_h)
    panel.text(pad, 12, caption, TEXT, scale=3)
    panel.text(pad, 36, subcaption, DIM_TEXT, 2)
    for index, value in enumerate(values):
        y, x = divmod(index, SIZE)
        if not (x0 <= x < x0 + cols and y0 <= y < y0 + rows):
            continue
        color = color_of(value)
        left, upper = pad + (x - x0) * scale, top + (y - y0) * scale
        if style == "cells":
            panel.rect(left, upper, scale - (1 if scale >= 6 else 0), scale - (1 if scale >= 6 else 0),
                       color or UNLIT)
        elif color is None:
            panel.disc(left + scale / 2, upper + scale / 2, scale * 0.22, UNLIT)
        else:
            panel.disc(left + scale / 2, upper + scale / 2, scale * 0.42, color, glow=0.35)
        if scale >= 24 and value:  # zoomed in far enough to print each cell's value
            label = str(value)
            size = 2 if scale < 40 else 3
            width = (4 * len(label) - 1) * size
            panel.text(left + (scale - width) // 2, upper + (scale - 5 * size) // 2, label,
                       BACKGROUND, size)
    key(panel, pad, top + rows * scale + 10)
    return panel


def energy_key(vmax, log):
    def draw(panel, x, y):
        width = min(200, panel.width - 2 * x - 60)
        for i in range(width):
            panel.rect(x + i, y, 1, 10, ramp(i / max(1, width - 1)))
        panel.text(x + width + 8, y, f"0-{vmax}" + (" LOG" if log else ""), DIM_TEXT, 2)
    return draw


def picture_of(snapshot, args, vmax, label):
    pixels = snapshot["pixels"]
    scale_fn = (lambda v: math.log1p(v) / math.log1p(vmax)) if args.log else (lambda v: v / vmax)
    lit = sum(1 for p in pixels if p)
    return render(
        pixels, args,
        lambda v: ramp(0.08 + 0.92 * scale_fn(v)) if v else None,
        f"V {snapshot['version']}", f"{label}MAX {max(pixels)}  LIT {lit}", energy_key(vmax, args.log),
    )


def diff_picture(before, after, args):
    delta = [a - b for a, b in zip(after["pixels"], before["pixels"])]
    peak = max(1, max(abs(d) for d in delta))

    def color_of(d):
        if d == 0:
            return None
        return ramp(0.35 + 0.65 * abs(d) / peak, [(0, UNLIT), (1, GAIN if d > 0 else LOSS)])

    def key(panel, x, y):
        panel.rect(x, y, 10, 10, GAIN)
        panel.text(x + 16, y, "GAINED", DIM_TEXT, 2)
        panel.rect(x + 80, y, 10, 10, LOSS)
        panel.text(x + 96, y, "LOST", DIM_TEXT, 2)

    changed = sum(1 for d in delta if d)
    return render(delta, args, color_of,
                  f"V {before['version']} - {after['version']}",
                  f"CHANGED {changed}  MAX DELTA {peak}", key)


def fetch(url, wall):
    with urllib.request.urlopen(f"{url.rstrip('/')}/walls/{wall}", timeout=5) as response:
        return json.load(response)


def load(source):
    text = sys.stdin.read() if source == "-" else open(source).read()
    return validate(json.loads(text), source)


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("inputs", nargs="*", help="snapshot JSON files, or - for stdin")
    parser.add_argument("-o", "--output", help="PNG path (default: first input with .png)")
    parser.add_argument("--url", help="server base URL, fetch live instead of reading files")
    parser.add_argument("--wall", help="wall ID to fetch with --url")
    parser.add_argument("--frames", type=int, default=1, help="with --url: number of snapshots")
    parser.add_argument("--interval", type=float, default=1.0, help="with --frames: seconds apart")
    parser.add_argument("--diff", action="store_true", help="two inputs: show after minus before")
    parser.add_argument("--style", choices=("leds", "cells"), default="leds")
    parser.add_argument("--scale", type=int, default=8, help="output pixels per wall cell")
    parser.add_argument("--vmax", type=int, help="energy mapped to the brightest colour")
    parser.add_argument("--log", action="store_true", help="logarithmic brightness")
    parser.add_argument("--columns", type=int, default=4, help="panels per row for sequences")
    parser.add_argument("--crop", default="0,0,64,64", help="X,Y,W,H region to draw")
    args = parser.parse_args()
    try:
        args.crop = [int(n) for n in args.crop.split(",")]
        x, y, w, h = args.crop
        assert 0 <= x and 0 <= y and w >= 1 and h >= 1 and x + w <= SIZE and y + h <= SIZE
    except (ValueError, AssertionError):
        parser.error("--crop must be X,Y,W,H inside the 64×64 wall")

    if args.url:
        if not args.wall:
            parser.error("--url requires --wall")
        snapshots = []
        for frame in range(args.frames):
            if frame:
                time.sleep(args.interval)
            snapshots.append(validate(fetch(args.url, args.wall), args.url))
        default_out = f"{args.wall}.png"
    elif args.inputs:
        snapshots = [load(path) for path in args.inputs]
        first = args.inputs[0]
        default_out = "wall.png" if first == "-" else first.rsplit(".", 1)[0] + ".png"
    else:
        parser.error("give snapshot files, - for stdin, or --url with --wall")

    if args.diff:
        if len(snapshots) != 2:
            parser.error("--diff takes exactly two snapshots: before after")
        panels = [diff_picture(*snapshots, args)]
    else:
        vmax = args.vmax or max(1, max(max(s["pixels"]) for s in snapshots))
        panels = [picture_of(s, args, vmax, f"{i + 1}/{len(snapshots)}  " if len(snapshots) > 1 else "")
                  for i, s in enumerate(snapshots)]

    columns = min(args.columns, len(panels))
    rows = math.ceil(len(panels) / columns)
    w, h = panels[0].width, panels[0].height
    sheet = Canvas(w * columns, h * rows)
    for i, panel in enumerate(panels):
        sheet.paste(panel, (i % columns) * w, (i // columns) * h)
    output = args.output or default_out
    with open(output, "wb") as f:
        f.write(sheet.png())
    print(f"wrote {output} ({sheet.width}x{sheet.height}, {len(panels)} panel(s))")


if __name__ == "__main__":
    main()
