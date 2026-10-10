// Uploads the built desktop installers to the public Vercel Blob store so the
// landing page (clusterheart.com) can link to them directly — no server needed.
//
// Each file gets a STABLE, deterministic pathname (addRandomSuffix: false +
// allowOverwrite: true), so the public URL never changes between releases and
// the links baked into vercel.json / index.html keep working.
//
// Usage:  BLOB_READ_WRITE_TOKEN=... node scripts/upload-blob.mjs <dir>
// where <dir> holds the collected *.dmg / *.exe / *.AppImage installers.

import { put } from '@vercel/blob'
import { readdirSync, readFileSync, statSync, appendFileSync } from 'node:fs'
import path from 'node:path'

const dir = process.argv[2]
if (!dir) {
  console.error('usage: node scripts/upload-blob.mjs <dir>')
  process.exit(1)
}

const token = process.env.BLOB_READ_WRITE_TOKEN
if (!token) {
  console.error('BLOB_READ_WRITE_TOKEN is not set')
  process.exit(1)
}

// Map a built installer filename to the stable public name we publish it under.
// electron-builder names things like Clusterheart-1.0.0-mac-arm64.dmg.
function friendlyName(file) {
  const f = file.toLowerCase()
  if (f.endsWith('.dmg') && f.includes('arm64')) return 'Clusterheart-macOS-AppleSilicon.dmg'
  if (f.endsWith('.dmg') && (f.includes('x64') || f.includes('x86_64'))) return 'Clusterheart-macOS-Intel.dmg'
  if (f.endsWith('.exe')) return 'Clusterheart-Windows.exe'
  if (f.endsWith('.appimage') && f.includes('arm64')) return 'Clusterheart-Linux-arm64.AppImage'
  if (f.endsWith('.appimage')) return 'Clusterheart-Linux.AppImage'
  return null // skip blockmaps, debs, anything we don't publish
}

const contentTypes = {
  '.dmg': 'application/x-apple-diskimage',
  '.exe': 'application/vnd.microsoft.portable-executable',
  '.appimage': 'application/x-executable',
}

const files = readdirSync(dir).filter((f) => statSync(path.join(dir, f)).isFile())

const published = []
for (const file of files) {
  const name = friendlyName(file)
  if (!name) continue

  const ext = path.extname(name).toLowerCase()
  const body = readFileSync(path.join(dir, file))
  const pathname = `downloads/${name}`

  console.log(`→ uploading ${file}  (${(body.length / 1e6).toFixed(1)} MB)  as  ${pathname}`)
  const res = await put(pathname, body, {
    access: 'public',
    addRandomSuffix: false,
    allowOverwrite: true,
    multipart: true, // installers are large; upload in parts
    contentType: contentTypes[ext],
    token,
  })
  console.log(`  ✓ ${res.url}`)
  published.push({ name, url: res.url, size: body.length })
}

if (published.length === 0) {
  console.error('No publishable installers found in ' + dir)
  process.exit(1)
}

// Surface the public URLs in the GitHub Actions run summary.
if (process.env.GITHUB_STEP_SUMMARY) {
  const rows = published
    .map((p) => `| \`${p.name}\` | ${(p.size / 1e6).toFixed(1)} MB | ${p.url} |`)
    .join('\n')
  appendFileSync(
    process.env.GITHUB_STEP_SUMMARY,
    `\n## Published installers\n\n| File | Size | Public URL |\n| --- | --- | --- |\n${rows}\n`
  )
}

console.log(`\nDone — ${published.length} installer(s) published to Vercel Blob.`)
