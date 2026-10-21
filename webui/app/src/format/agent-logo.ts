// SPDX-License-Identifier: AGPL-3.0-or-later

/** The official mark each agent CLI is known by, keyed by the registry's own
 *  name (the same key `/api/agents` returns).
 *
 *  The roster draws people, and a person's face is the one thing a reader
 *  recognises before reading the name — an initial in a coloured circle is a
 *  placeholder, not a face. The marks live in `public/agents/` and are copied
 *  verbatim from the vendor artwork: no recolouring, no redrawing, because a
 *  half-remembered logo is worse than an honest initial.
 *
 *  `plate` is the background the mark needs, not a theme preference. Marks
 *  drawn in near-black ink (Claude Code, OpenCode, Pi, …) only read on a light
 *  plate; Grok's and ZCode's are near-white and only read on a dark one. The
 *  plate therefore travels with the file and looks the same in both themes —
 *  an app icon, not a tinted chip.
 *
 *  A name with no entry here (a CLI the console has never seen) falls back to
 *  the initial avatar at the call site, so adding an agent never breaks the
 *  roster; it only means the new hire starts without a photo. */
export interface AgentLogo {
  /** File under `public/agents/`. */
  file: string
  /** The plate the artwork is drawn for. */
  plate: 'light' | 'dark'
}

const LOGOS: Record<string, AgentLogo> = {
  antigravity: { file: 'antigravity.svg', plate: 'light' },
  claude_code: { file: 'claude_code.svg', plate: 'light' },
  codex: { file: 'codex.svg', plate: 'light' },
  deepseek_harness: { file: 'deepseek_harness.svg', plate: 'light' },
  grok_build: { file: 'grok_build.svg', plate: 'dark' },
  hermes: { file: 'hermes.svg', plate: 'light' },
  openclaw: { file: 'openclaw.svg', plate: 'light' },
  opencode: { file: 'opencode.svg', plate: 'light' },
  pi: { file: 'pi.svg', plate: 'light' },
  zcode: { file: 'zcode.svg', plate: 'dark' },
}

/** agentLogo returns the mark for an agent, or undefined when the console has
 *  no official artwork for it. */
export function agentLogo(name: string): AgentLogo | undefined {
  return LOGOS[name]
}
