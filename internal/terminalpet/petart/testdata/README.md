# Reference frames

`parity.jsonl.gz` holds frames drawn by the companion art engine in
[OpenClaude](https://github.com/Gitlawb/openclaude) (`src/buddy/heroCanvas.ts`,
`heroForms.ts`, `heroMoods.ts`, `heroArt.ts`, `heroImage.ts`), at commit
`0c6b6f9e`. This package is a Go port of that engine, and
`parity_test.go` checks that both draw the same pictures.

Each line is one JSON record:

- `kind: "frame"`: palette-key rows for one character, mood, time and scale
  (0.5 for the half-block sprite, 1 for the full design).
- `kind: "image"`: straight-alpha RGBA bytes, base64, for the anti-aliased
  image at 40 pixels tall, with its supersampling (4 is the default, 2 is
  what animation frames use).

OpenClaude stores three characters under older ids. The exporter writes the
ids this package uses: `robinhood` is `robin`, `kaio` is `nova` and `strawhat`
is `inky`.

## Regenerating

Change the art in both engines, then export again from an OpenClaude checkout
(needs [Bun](https://bun.sh)):

```sh
cd /path/to/openclaude
bun run /path/to/zero/internal/terminalpet/petart/testdata/export_parity.ts \
  /path/to/zero/internal/terminalpet/petart/testdata/parity.jsonl.gz
```

Update the commit above when you do.
