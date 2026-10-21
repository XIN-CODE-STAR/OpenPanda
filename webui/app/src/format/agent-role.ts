// SPDX-License-Identifier: AGPL-3.0-or-later

/** The jobs an agent can be hired for, as the composer's roster card prints
 *  them.
 *
 *  The registry describes each agent with routing tags — "multi_file_edits",
 *  "running_tests", "long_running_tasks" — which are precise for a scorer and
 *  unreadable in a list of people. This folds those tags into a handful of
 *  roles, so the picker can say what someone is for instead of echoing a
 *  snake_case token. The registry stays the source of truth: a tag we do not
 *  know is dropped rather than shown raw, and an agent whose tags are all
 *  unknown falls back to its binary name at the call site. */
export type AgentRole =
  | 'code'
  | 'search'
  | 'review'
  | 'test'
  | 'shell'
  | 'longrun'
  | 'reason'

const TAGS: Record<string, AgentRole> = {
  multi_file_edits: 'code',
  quick_edits: 'code',
  fast_scripts: 'code',
  refactoring: 'code',
  code_generation: 'code',
  scripting: 'code',
  code_search: 'search',
  code_explanation: 'search',
  code_review: 'review',
  running_tests: 'test',
  build_diagnostics: 'test',
  shell_execution: 'shell',
  automation: 'shell',
  long_running_tasks: 'longrun',
  complex_reasoning: 'reason',
}

/** agentRoles folds an agent's best-at tags into de-duplicated roles, in the
 *  order the agent lists them — the registry's own priority survives. */
export function agentRoles(bestAt?: string[]): AgentRole[] {
  const out: AgentRole[] = []
  for (const tag of bestAt ?? []) {
    const role = TAGS[tag.trim().toLowerCase()]
    if (role && !out.includes(role)) out.push(role)
  }
  return out
}

/** agentInitial is the avatar letter for a roster row: the first character of
 *  the display name, upper-cased for latin names. It reads the code point, not
 *  the UTF-16 unit, so a CJK or emoji leading character is not half a glyph. */
export function agentInitial(name: string): string {
  const first = Array.from(name.trim())[0] ?? '?'
  return first.toLocaleUpperCase()
}

/** agentTint maps an agent name to one of the avatar tints. It is a hash, not
 *  a random: the same employee must wear the same colour every time the card
 *  opens, and two agents on one card should rarely collide. */
export function agentTint(name: string, buckets = 5): number {
  let h = 0
  for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) >>> 0
  return h % buckets
}
