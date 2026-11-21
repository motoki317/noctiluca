# Logo and colors

The mark is a round cell with one blue dot seated in a groove at its lower left. It reads as a cell of the sea sparkle (*Noctiluca scintillans*), the plankton that glows at night, and as a thought bubble, for an agent that lives in chat and dreams at night. The dot is the only saturated color in the identity.

The mark is drawn from three circles whose centers lie on one 45° line. With the radius of the cell as 6 units:

- The dot has a radius of 2 units, and its center is 7 units from the center of the cell.
- The groove is a circle of radius 3 units around the center of the dot, cut out of the cell. It leaves a gap of 1 unit between the dot and the cell.

On a dark background, a light shape looks larger than the same shape in a dark color on a light background. The files drawn for dark backgrounds (`logo-dark.svg`, `lockup-dark.svg`, and `avatar.svg`, which has a Night background) compensate: the cell and the dot are 0.05 units smaller in radius, and the wordmark has a lower weight.

The wordmark and the tagline are set in [Fraunces](https://github.com/undercasetype/Fraunces) and stored as outlines, so no file needs the font. No script regenerates the outlines. The wordmark uses weight 600 (580 in `lockup-dark.svg`) and optical size 72, with letter spacing adjusted by hand.

| Name | Color | Use |
|---|---|---|
| Ink | `#0E1A2B` | Cell and wordmark on light backgrounds |
| Pale | `#E9EEF6` | Cell and wordmark on dark backgrounds |
| Blue | `#1677E5` | Dot on light backgrounds |
| Bright blue | `#4DA8FF` | Dot on dark backgrounds |
| Night | `#0B1524` | Background of the avatar |
| Mist | `#A9B6CA` | Tagline on the social images |

## Files

The logo and lockup files have transparent backgrounds.

- `assets/logo.svg` and `assets/logo-dark.svg`: the mark.
- `assets/lockup.svg` and `assets/lockup-dark.svg`: the mark with the wordmark.
- `assets/avatar.svg` and `assets/avatar.png` (512×512): the mark on a Night background, for chat accounts. The mark stays whole when a chat platform crops the image to a circle.
- `assets/social-2x1.png` (1280×640): the GitHub social preview. GitHub does not read it from the repository. After the file changes, upload it again under the repository's Settings → General → Social preview.
- `assets/social-16x9.png` (1280×720): the image at the top of the README.

## Social images

A social image shows the mark as a raised relief on dark paper, rendered in Blender Cycles, with the lockup and the tagline beside it. To rebuild both images, run:

```sh
assets/social/build.sh
```

The script needs `nix` and `uv`. It takes about 20 seconds on a 24-thread CPU, after `nix` downloads Blender on the first run. Two builds on the same machine wrote the same bytes. Builds on different machines have not been compared.

For each size, the script runs two steps:

1. `assets/social/scene.py` renders the relief. It reads the outline of the relief and the color of the dot from `assets/logo-dark.svg`.
2. `assets/social/compose.py` places `assets/lockup-dark.svg` and `assets/social/tagline.svg` on the render, then adds film grain.

`assets/social/layout.py` holds the positions and sizes of the relief, the lockup, and the tagline.

`assets/social/tagline.svg` holds the tagline as outlines. To change the tagline:

1. Set the new text in Fraunces at a font size of 44 px, with weight 500, optical size 30, SOFT 0, and WONK 0. Use 0.004 em letter spacing and 58 px between baselines.
2. Convert the text to outlines in the coordinates of the 1280×640 image, with the first baseline at y = 362.
3. Replace the paths in `tagline.svg`. The `viewBox` must start at `TAGLINE_VIEWPORT_LEFT_PX` and `TAGLINE_VIEWPORT_TOP_PX` from `layout.py` and cover the new paths. The `width` and `height` must equal the size of the `viewBox`.
