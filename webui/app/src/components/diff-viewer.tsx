// SPDX-License-Identifier: AGPL-3.0-or-later

/** Shared unified-diff renderer — used by the session worktree drawer and
 *  the self-merge patch-set review card. Line classification is deliberately
 *  minimal: add/del/hunk/meta, no intra-line highlighting. */
export function DiffViewer({ patch }: { patch: string }) {
  if (!patch) return null
  const lines = patch.split('\n')
  return (
    <pre class="diff-patch diff-patch-view mono">
      {lines.map((line, idx) => {
        let cls = 'diff-line'
        if (line.startsWith('+') && !line.startsWith('+++')) {
          cls += ' diff-line-add'
        } else if (line.startsWith('-') && !line.startsWith('---')) {
          cls += ' diff-line-del'
        } else if (line.startsWith('@@')) {
          cls += ' diff-line-hunk'
        } else if (line.startsWith('diff ') || line.startsWith('index ') || line.startsWith('---') || line.startsWith('+++')) {
          cls += ' diff-line-meta'
        }
        return (
          <div key={idx} class={cls}>
            <span class="diff-line-content">{line || ' '}</span>
          </div>
        )
      })}
    </pre>
  )
}
