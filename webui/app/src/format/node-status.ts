// SPDX-License-Identifier: AGPL-3.0-or-later

/** Node status → "is this device reachable right now".
 *
 *  The panel's ledger writes `online` / `offline`, but the same fact reaches
 *  the console from three places (heartbeats, the node directory, paired
 *  peers) and a re-worded status used to mean a grey dot on a perfectly
 *  healthy node. Views ask this one function instead of comparing the raw
 *  string, so a dot, a badge and a routing filter can never disagree. */

const CONNECTED = new Set(['online', 'connected', 'running', 'active', 'ok'])

/** True when the status says the node is connected. Unknown/empty is false:
 *  a dot is a claim, so anything we cannot read stays dark. */
export function isNodeConnected(status: string | null | undefined): boolean {
  if (!status) return false
  return CONNECTED.has(status.trim().toLowerCase())
}

/** The parts of GET /api/self that answer "is my node up". Structural, so the
 *  helper stays testable without the whole payload. */
export interface SelfNodeState {
  node_running?: boolean
  node?: { status?: string; running?: boolean }
}

/** Is this machine's node up right now?
 *
 *  `node_running` is the daemon's identity lock, and only `panda daemon`
 *  takes it: the web console serves the node in-process (`MeshNode: true`)
 *  and never locks, so on a console-driven install the flag is false while
 *  the node is perfectly online — a grey dot next to a live node. The ledger
 *  row the node registers (status online, running) is the evidence that
 *  holds in both shapes, so either signal lights the dot. */
export function isSelfNodeUp(self: SelfNodeState | null | undefined): boolean {
  if (!self) return false
  if (self.node_running) return true
  const row = self.node
  if (!row) return false
  return isNodeConnected(row.status) && row.running !== false
}
