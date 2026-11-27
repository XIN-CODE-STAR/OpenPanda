// SPDX-License-Identifier: AGPL-3.0-or-later

// The thread a rendered block belongs to. Relative file paths inside a turn —
// an attachment pill, a screenshot a tool row previews — are relative to that
// thread's own worktree, not to the node's work_path, and /api/fs/image needs
// the session id to resolve them (and to allow a worktree that lives outside
// work_path at all).
//
// A context rather than a prop: the value is needed at the leaf (ImagePreview)
// but only known at the top (the thread view), and the two are four components
// apart. Anything rendered outside a thread sees null and keeps the old
// behaviour.

import { createContext } from 'preact'

export const SessionScope = createContext<string | null>(null)
