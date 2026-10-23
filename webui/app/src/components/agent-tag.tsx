// SPDX-License-Identifier: AGPL-3.0-or-later

/** An agent's name, as a chip.
 *
 *  An agent name is an identity, not a value. Rendered as plain or as a grey
 *  mono chip it read exactly like the file paths and ids beside it, and those
 *  answer a different question ("what did it touch" vs "who ran this"). The
 *  pill gives it the console's second accent — the one hue the palette keeps
 *  for "a different kind of thing" — and keeps the mono face, so the name is
 *  still recognisably a machine name rather than prose.
 *
 *  Used wherever an agent appears as a *field*: the brief's allow-list, the
 *  trace's execution step, the result's agent chip. A title that happens to
 *  contain an agent name ("测试 agent:deepseek_harness 调用") stays text — it
 *  is a sentence, not an identity. */
export function AgentTag({ name }: { name: string }) {
  return (
    <span class="agent-tag" title={name}>
      {name}
    </span>
  )
}
