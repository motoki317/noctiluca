from io import BytesIO
from pathlib import Path
import subprocess
import sys
import xml.etree.ElementTree as ET

import numpy as np
import png
from PIL import Image

import layout

HERE = Path(__file__).resolve().parent
SVG_NS = "http://www.w3.org/2000/svg"
ET.register_namespace("", SVG_NS)


def overlay_svg(height):
    root = ET.Element(f"{{{SVG_NS}}}svg", width=str(layout.CANVAS_WIDTH_PX), height=str(height))
    offset_px = layout.type_y_offset_px(height)

    lockup = ET.parse(HERE.parent / "lockup-dark.svg").getroot()
    scale = layout.LOCKUP_SVG_TO_CANVAS_SCALE
    lockup.set("x", str(layout.LOCKUP_VIEWPORT_LEFT_PX))
    lockup.set("y", str(layout.LOCKUP_VIEWPORT_TOP_PX + offset_px))
    for attr in ("width", "height"):
        lockup.set(attr, f"{float(lockup.attrib[attr]) * scale:.3f}")
    root.append(lockup)

    tagline = ET.parse(HERE / "tagline.svg").getroot()
    tagline.set("x", str(layout.TAGLINE_VIEWPORT_LEFT_PX))
    tagline.set("y", str(layout.TAGLINE_VIEWPORT_TOP_PX + offset_px))
    root.append(tagline)

    return ET.tostring(root, encoding="utf-8")


def compose(scene_png, output):
    with Path(scene_png).open("rb") as render:
        width, height, rows, info = png.Reader(file=render).read()
        if info["bitdepth"] != 16 or info["planes"] != 3:
            raise ValueError("Expected a 16-bit RGB Blender render")
        scene = np.array(list(rows), dtype=np.float64).reshape(height, width, 3) / 65535.0

    overlay_png = subprocess.run(
        ["resvg", "--skip-system-fonts", "--resources-dir", str(HERE), "-", "-c"],
        input=overlay_svg(height),
        stdout=subprocess.PIPE,
        check=True,
    ).stdout
    with Image.open(BytesIO(overlay_png)) as overlay:
        rgba = np.asarray(overlay.convert("RGBA"), dtype=np.float64) / 255.0
    alpha = rgba[..., 3:4]
    img = scene * (1 - alpha) + rgba[..., :3] * alpha

    # Soften grain and taper it on lighter tones to keep the type clean.
    rng = np.random.default_rng(472)
    grain = rng.normal(0.0, 1.0, (height, width))
    grain = (grain + 0.5 * (np.roll(grain, 1, 0) + np.roll(grain, 1, 1))) / 1.5
    luma = img @ np.array([0.2126, 0.7152, 0.0722])
    amount = 1.7 / 255.0 * (1.0 - np.clip(luma * 1.6, 0, 1)) ** 2
    img += (grain * amount)[..., None]

    # Triangular dither keeps 8-bit quantisation from banding in the shadows.
    dither = (rng.random((height, width, 1)) - rng.random((height, width, 1))) / 255.0
    out = np.clip(np.round((img + dither) * 255.0), 0, 255).astype(np.uint8)
    Image.fromarray(out).save(output, optimize=True)


if __name__ == "__main__":
    compose(*sys.argv[1:])
