// SPDX-License-Identifier: AGPL-3.0-or-later

/** Where a relative `TERMS.ja.md` should point once it is inside the panel —
 *  the console serves no such path, so the sibling-translation links in the
 *  document would otherwise be dead. */
export const TERMS_REPO_BLOB = 'https://github.com/Xustalis/OpenPanda/blob/main'

/** Turn one of the repository's TERMS files into the markdown the consent card
 *  renders. Three adjustments, no edits to the prose:
 *
 *  - the file's own H1 is dropped — the modal already carries the title;
 *  - CRLF is normalised, so the parser sees one line-ending regardless of how
 *    the file was checked out;
 *  - links to sibling translations are pointed at the repository.
 *
 *  Pure and exported so the transform is testable without Vite's `?raw`. */
export function prepareTerms(raw: string): string {
  return raw
    .replace(/\r\n/g, '\n')
    .replace(/^# .*\n+/, '')
    .replace(/\]\((TERMS[\w.-]*\.md)\)/g, `](${TERMS_REPO_BLOB}/$1)`)
}
