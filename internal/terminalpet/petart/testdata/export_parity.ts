// Exports reference frames from OpenClaude's companion art engine, which this
// package is a port of. Run it from an OpenClaude checkout:
//
//   cd /path/to/openclaude
//   bun run /path/to/zero/internal/terminalpet/petart/testdata/export_parity.ts \
//     /path/to/zero/internal/terminalpet/petart/testdata/parity.jsonl.gz
//
// See README.md in this directory.

import { gzipSync } from 'node:zlib'
import { writeFileSync } from 'node:fs'

const root = process.cwd()
const { renderHero } = await import(`${root}/src/buddy/heroArt.ts`)
const { renderHeroImage } = await import(`${root}/src/buddy/heroImage.ts`)
const { HERO_MOODS } = await import(`${root}/src/buddy/heroMoods.ts`)

// OpenClaude's stored ids, and the ids this package uses.
const FORMS: Array<[string, string]> = [
  ['twig', 'twig'],
  ['robinhood', 'robin'],
  ['kaio', 'nova'],
  ['strawhat', 'inky'],
  ['merlin', 'merlin'],
  ['kage', 'kage'],
  ['ember', 'ember'],
  ['corsair', 'corsair'],
  ['keynote', 'keynote'],
]

const SPRITE_TIMES = [0, 0.37, 1.3, 2.9, 4.6, 6.1]
const FULL_TIMES = [0.37, 4.6]
const IMAGE_TIME = 1.3
const IMAGE_HEIGHT = 40
// Supersampling per mood: 4 is the engine's default, 2 is what animation
// frames use.
const IMAGE_MOODS = new Map([
  ['idle', 2],
  ['thinking', 4],
  ['celebrate', 2],
  ['action', 4],
])

const lines: string[] = []
for (const [source, form] of FORMS) {
  for (const mood of HERO_MOODS) {
    for (const t of SPRITE_TIMES) {
      lines.push(JSON.stringify({ kind: 'frame', form, mood, t, scale: 0.5, rows: renderHero(source, mood, t, 0.5) }))
    }
    for (const t of FULL_TIMES) {
      lines.push(JSON.stringify({ kind: 'frame', form, mood, t, scale: 1, rows: renderHero(source, mood, t, 1) }))
    }
    const supersample = IMAGE_MOODS.get(mood)
    if (supersample === undefined) continue
    const image = renderHeroImage(source, mood, IMAGE_TIME, IMAGE_HEIGHT, supersample)
    lines.push(
      JSON.stringify({
        kind: 'image',
        form,
        mood,
        t: IMAGE_TIME,
        height: IMAGE_HEIGHT,
        supersample,
        width: image.width,
        rgba: Buffer.from(image.rgba).toString('base64'),
      }),
    )
  }
}

const out = process.argv[2]
if (!out) throw new Error('usage: export_parity.ts <output.jsonl.gz>')
writeFileSync(out, gzipSync(`${lines.join('\n')}\n`, { level: 9 }))
console.log(`${lines.length} records -> ${out}`)
