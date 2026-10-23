// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

import { prepareTerms, TERMS_REPO_BLOB } from './terms-text.ts'

// The consent card renders the repository's TERMS files directly, so the only
// logic worth guarding is the transform that adapts them to the panel — and the
// promise that each file still has the section count the card was built for.
const FILES: Record<string, string> = {
  en: '../../../../TERMS.md',
  'zh-CN': '../../../../TERMS.zh-CN.md',
  ja: '../../../../TERMS.ja.md',
  es: '../../../../TERMS.es.md',
  de: '../../../../TERMS.de.md',
}

const raw = (file: string) => readFileSync(new URL(file, import.meta.url), 'utf8')

describe('prepareTerms', () => {
  it('drops the file’s own H1, because the modal carries the title', () => {
    const out = prepareTerms('# Title\n\n## 1. First\n\nBody.\n')
    assert.ok(!out.includes('# Title'))
    assert.ok(out.startsWith('## 1. First'))
  })

  it('normalises CRLF so the parser sees one line-ending', () => {
    assert.ok(!prepareTerms('# T\r\n\r\n## 1. A\r\n\r\nBody.\r\n').includes('\r'))
  })

  it('points sibling-translation links at the repository', () => {
    const out = prepareTerms('# T\n\nSee [日本語](TERMS.ja.md) and [English](TERMS.md).\n')
    assert.ok(out.includes(`](${TERMS_REPO_BLOB}/TERMS.ja.md)`))
    assert.ok(out.includes(`](${TERMS_REPO_BLOB}/TERMS.md)`))
    assert.ok(!out.includes('](TERMS.ja.md)'))
  })

  it('leaves other links alone', () => {
    const out = prepareTerms('# T\n\n[repo](https://example.com/x.md) [doc](docs/x.md)\n')
    assert.ok(out.includes('](https://example.com/x.md)'))
    assert.ok(out.includes('](docs/x.md)'))
  })
})

describe('the shipped terms files', () => {
  for (const [locale, file] of Object.entries(FILES)) {
    it(`${locale} carries the seventeen sections the card renders`, () => {
      const out = prepareTerms(raw(file))
      const sections = out.split('\n').filter((l) => l.startsWith('## '))
      assert.equal(sections.length, 17, `${locale}: ${sections.length} sections`)
      // No leftover H1: the card's own heading is the only title on screen.
      assert.equal(
        out.split('\n').filter((l) => /^# /.test(l)).length,
        0,
        `${locale}: stray H1`,
      )
      // Every section is numbered, which is what makes the count checkable.
      assert.match(sections[0] ?? '', /^## 1\. /)
    })
  }
})
