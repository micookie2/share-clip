#!/usr/bin/env python3
"""Regenerate share-clip's raster icon assets from assets/icon.svg.

`icon.svg` is the single source of truth. This script writes the PNG sizes and
the multi-size `icon.ico` that the server embeds and the Windows builds link
in. Run it only when the vector icon changes:

    python3 assets/generate.py

Requirements (maintainer tool only, not needed to build the project):

    pip install cairosvg pillow

After regenerating, refresh the Windows resources too:

    make icon-windows
"""
import io
from pathlib import Path

import cairosvg
from PIL import Image

HERE = Path(__file__).resolve().parent
SVG = HERE / "icon.svg"
ICO = HERE / "icon.ico"

# (pixel size, output file)
PNGS = [
    (512, HERE / "icon-512.png"),
    (192, HERE / "icon-192.png"),
    (180, HERE / "apple-touch-icon.png"),
]

# Windows/web .ico sizes, largest first.
ICO_SIZES = [(16, 16), (24, 24), (32, 32), (48, 48), (64, 64), (128, 128), (256, 256)]


def render(size: int) -> Image.Image:
    png = cairosvg.svg2png(bytestring=SVG.read_bytes(), output_width=size, output_height=size)
    return Image.open(io.BytesIO(png)).convert("RGBA")


def main() -> None:
    for size, out in PNGS:
        render(size).save(out)
        print(f"wrote {out.relative_to(HERE.parent)} ({size}x{size})")

    # Pillow downsamples the 256px render with LANCZOS for the smaller frames.
    render(256).save(ICO, sizes=ICO_SIZES)
    print(f"wrote {ICO.relative_to(HERE.parent)} {ICO_SIZES}")


if __name__ == "__main__":
    main()
