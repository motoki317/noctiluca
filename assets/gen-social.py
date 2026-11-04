"""Social image for a logo candidate: a night sea whose wave crest glows with plankton.

usage: python3 -I gen-social.py <width> <height> > social.svg
Run from assets/
"""
import math, random, re, sys

cand, W, H = "b", int(sys.argv[1]), int(sys.argv[2])
random.seed(11)
TAGLINE = "A minimal agent harness that sleeps, dreams, and learns."

logo = open("logo.svg").read()
inner = logo.split(">", 1)[1].rsplit("</svg>", 1)[0]
# The mark's box in logo units (cell plus tentacle, without halo or edge), and the cell itself.
MARK = {"b": ((52, 28, 204, 236), (128, 104, 76)), "c": ((50, 26, 206, 238), (128, 104, 78)),
        "d": ((50, 34, 206, 225), (128, 88, 78)), "e": ((54, 46, 202, 206), (128, 124, 74))}[cand]
inner = re.sub(r"<!--.*?-->", "", inner, flags=re.S)


# The front wave as cubic segments in (x/W, y/H): a long back, a crest that curls
# forward into a lip, then the face falling to the trough.
WAVE = [(-0.02, 0.88),
        (0.25, 0.88), (0.50, 0.83), (0.64, 0.72),
        (0.70, 0.67), (0.775, 0.665), (0.795, 0.705),
        (0.805, 0.73), (0.785, 0.75), (0.768, 0.738),
        (0.758, 0.765), (0.772, 0.815), (0.83, 0.85),
        (0.89, 0.885), (0.96, 0.89), (1.03, 0.885)]
CREST = (0.62, 0.8)  # x/W range where the water is disturbed and glows most


def cubic(p0, p1, p2, p3, t):
    m = 1 - t
    return tuple(m**3 * a + 3 * m * m * t * b + 3 * m * t * t * c + t**3 * d
                 for a, b, c, d in zip(p0, p1, p2, p3))


pts = [(x * W, y * H) for x, y in WAVE]
front_d = f"M{pts[0][0]:.1f} {pts[0][1]:.1f} " + " ".join(
    "C" + " ".join(f"{x:.1f} {y:.1f}" for x, y in pts[k:k + 3]) for k in range(1, len(pts), 3))
samples = []  # (x, y, nx, ny): points on the wave with the normal pointing into the water
for k in range(0, len(pts) - 1, 3):
    seg = pts[k:k + 4]
    for n in range(200):
        a, b = cubic(*seg, n / 200), cubic(*seg, (n + 1) / 200)
        dx, dy = b[0] - a[0], b[1] - a[1]
        ln = math.hypot(dx, dy) or 1
        samples.append((a[0], a[1], -dy / ln, dx / ln, ln))
total = sum(s[4] for s in samples)


def on_wave():
    r = random.uniform(0, total)
    for x, y, nx, ny, ln in samples:
        r -= ln
        if r <= 0:  # land anywhere along this short piece, not on its start point
            f = random.random()
            return x - nx * 0 + f * ln * ny, y + f * ln * -nx, nx, ny, ln
    return samples[-1]


back_d = (f"M{-0.02*W:.0f} {0.83*H:.0f} C{0.3*W:.0f} {0.80*H:.0f} {0.55*W:.0f} {0.81*H:.0f} "
          f"{0.75*W:.0f} {0.79*H:.0f} S{0.95*W:.0f} {0.80*H:.0f} {1.03*W:.0f} {0.81*H:.0f}")

# Plankton: dense on the crest, thinning into the water below it.
dots = []
while len(dots) < int(W * 0.55):
    x, y, nx, ny, _ = on_wave()
    hot = CREST[0] * W < x < CREST[1] * W
    if not hot and random.random() < 0.45:
        continue
    depth = abs(random.gauss(0, 0.022 * H * (1.4 if hot else 1)))
    x, y = x + nx * depth + random.uniform(-2, 2), y + ny * depth
    fade = depth / (0.06 * H)
    r = random.choice([0.9, 1.2, 1.5, 1.9, 2.4]) * (1.3 if hot and fade < 0.3 else 1)
    o = max(0.12, min(0.95, random.uniform(0.6, 1.0) - fade * 0.5))
    dots.append(f'<circle cx="{x:.1f}" cy="{y:.1f}" r="{r}" opacity="{o:.2f}"/>')
stars = []
for _ in range(34):
    x, y = random.uniform(0, W), random.uniform(0, H * 0.55)
    stars.append(f'<circle cx="{x:.0f}" cy="{y:.0f}" r="{random.choice([0.8, 1.1, 1.4])}" '
                 f'opacity="{random.uniform(0.15, 0.45):.2f}"/>')

# Layout. Everything important stays inside the central 80% of the canvas.
(mx0, my0, mx1, my1), (ccx, ccy, cr) = MARK
top_room = 0.66 * H  # top of the crest: the free sky ends here
if cand in ("b", "d", "e"):  # stacked, centred
    # px per logo unit: the mark is 0.31 of the height; the solid n carries more weight per unit
    k = (0.25 if cand == "e" else 0.31) * H / (my1 - my0)
    word, tag = 0.13 * H, 0.036 * H
    block = (my1 - my0) * k + 0.035 * H + word * 0.75 + 0.06 * H + tag * 0.75
    ly = max(0.1 * H, (top_room - block) / 2) - my0 * k
    lx = W / 2 - ccx * k
    wy = ly + my1 * k + 0.035 * H + word * 0.75
    ty = wy + 0.06 * H + tag * 0.75
    text = (f'<text x="{W/2:.0f}" y="{wy:.0f}" text-anchor="middle" font-size="{word:.0f}" '
            f'font-weight="300" letter-spacing="3">noctiluca</text>\n'
            f' <text x="{W/2:.0f}" y="{ty:.0f}" text-anchor="middle" font-size="{tag:.0f}" '
            f'font-weight="300" fill="#9CC7E4">{TAGLINE}</text>')
else:  # logo left of the words
    k = 0.34 * 640 / (my1 - my0)
    word, tag = 0.19 * 640, 0.04 * 640
    gap = 0.03 * W
    text_w = 0.52 * W  # measured from the rendered tagline at this size
    mark_w = (mx1 - mx0) * k
    x0 = (W - (mark_w + gap + text_w)) / 2
    lx = x0 - mx0 * k
    ly = (top_room - (my1 - my0) * k) / 2 + 0.02 * H - my0 * k
    tx = x0 + mark_w + gap
    cy = ly + ccy * k  # the words sit on the cell, not on the whole mark
    text = (f'<text x="{tx:.0f}" y="{cy + word * 0.12:.0f}" font-size="{word:.0f}" '
            f'font-weight="300" letter-spacing="2">noctiluca</text>\n'
            f' <text x="{tx + 4:.0f}" y="{cy + word * 0.12 + tag * 2.2:.0f}" font-size="{tag:.0f}" '
            f'font-weight="300" fill="#9CC7E4">{TAGLINE}</text>')

print(f'''<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" viewBox="0 0 {W} {H}">
<defs>
 <linearGradient id="s-sky" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#050A16"/><stop offset="1" stop-color="#0B1F3A"/></linearGradient>
 <linearGradient id="s-sea" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#0F3358"/><stop offset="0.5" stop-color="#081A33"/><stop offset="1" stop-color="#050A16"/></linearGradient>
 <linearGradient id="s-backsea" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#0B2746"/><stop offset="1" stop-color="#050A16"/></linearGradient>
 <filter id="s-blur" x="-10%" y="-200%" width="120%" height="500%"><feGaussianBlur stdDeviation="{0.014*H:.1f}"/></filter>
 <filter id="s-dot" x="-50%" y="-50%" width="200%" height="200%"><feGaussianBlur stdDeviation="1.6" result="b"/><feMerge><feMergeNode in="b"/><feMergeNode in="SourceGraphic"/></feMerge></filter>
</defs>
<rect width="{W}" height="{H}" fill="url(#s-sky)"/>
<g fill="#E6FDFF">{''.join(stars)}</g>
<path d="{back_d} L{W+40} {H} L-40 {H}Z" fill="url(#s-backsea)"/>
<path d="{back_d}" fill="none" stroke="#5CE1E6" stroke-width="1.2" opacity="0.18"/>
<path d="{front_d} L{W+40} {H} L-40 {H}Z" fill="url(#s-sea)"/>
<path d="{front_d}" fill="none" stroke="#5CE1E6" stroke-width="{0.03*H:.0f}" opacity="0.45" filter="url(#s-blur)"/>
<path d="{front_d}" fill="none" stroke="#8FF3F7" stroke-width="2" opacity="0.8"/>
<g fill="#8FF3F7" filter="url(#s-dot)">{''.join(dots)}</g>
<svg overflow="visible" x="{lx:.1f}" y="{ly:.1f}" width="{256*k:.1f}" height="{256*k:.1f}" viewBox="0 0 256 256">{inner}</svg>
<g font-family="Noto Sans CJK JP" fill="#E6FDFF">
 {text}
</g>
</svg>''')
