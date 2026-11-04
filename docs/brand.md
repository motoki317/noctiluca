# Logo and colors

The logo is one cell of the sea sparkle (*Noctiluca scintillans*). A round cell glows blue from a white nucleus at its center, and one tentacle hangs from its bottom. Only the dark background and the light express night and sleep. The social image is a night sea whose wave crests glow with sea sparkle.

| Name | Color | Use |
|---|---|---|
| Abyss | `#050A16` | Background (top) |
| Deep | `#0B1F3A` | Background (bottom) |
| Cell | `#1E4D78` | Inside the cell |
| Glow | `#5CE1E6` | Primary color. Tentacle, links |
| Spark | `#8FF3F7` | Light particles |
| Foam | `#E6FDFF` | Text, nucleus glow |
| Mist | `#9CC7E4` | Secondary text |

The cell edge uses `#36BCD6`, and the outline uses `#123A63`.

## Files

- `assets/logo.svg`: the logo (no background, 256×256). It works on both dark and white backgrounds.
- `assets/social-2x1.png` (1280×640) and `assets/social-16x9.png` (1280×720): the social images. Set the 2:1 image as the GitHub social preview.
- `assets/gen-social.py`: the script that generates the SVG of a social image. To regenerate an image, run `cd assets && python3 -I gen-social.py 1280 640 > social-2x1.svg && rsvg-convert social-2x1.svg -o social-2x1.png`. The text renders in Noto Sans CJK JP Light.
