// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// `/node` — the terminal twin of the console composer's node picker.
//
// The web console pins one turn's task to a node with AskScope.Node; the
// scheduler still falls back when the model emits its own spec.node. The
// terminal's pin is deliberately one-shot: it is consumed by the next ask, so
// a single "run this on the GPU box" cannot silently become a standing
// preference for every later prompt in the conversation. `panda ask --node`
// is the scriptable equivalent.

import (
	"strings"

	"github.com/Xustalis/OpenPanda/internal/i18n"
)

// setNextNode pins the next ask to node ("" clears).
func (r *repl) setNextNode(node string) {
	r.sessMu.Lock()
	r.nextNode = strings.TrimSpace(node)
	r.sessMu.Unlock()
}

// nextNodeNow reports the pending pin, for the status row.
func (r *repl) nextNodeNow() string {
	if r == nil {
		return ""
	}
	r.sessMu.Lock()
	defer r.sessMu.Unlock()
	return r.nextNode
}

// takeNextNode consumes the pin: the turn that reads it owns it.
func (r *repl) takeNextNode() string {
	r.sessMu.Lock()
	defer r.sessMu.Unlock()
	node := r.nextNode
	r.nextNode = ""
	return node
}

// nodeCommand applies one /node argument and returns the localized result.
// Shared by the classic REPL and the TUI so both say the same thing.
func (r *repl) nodeCommand(arg string) (string, error) {
	loc := r.locale()
	arg = strings.TrimSpace(arg)
	switch strings.ToLower(arg) {
	case "":
		if cur := r.nextNodeNow(); cur != "" {
			return i18n.Tf(loc, "repl.node.current", "node", cur), nil
		}
		return i18n.T(loc, "repl.node.auto"), nil
	case "auto", "clear", "none", "-", "reset":
		r.setNextNode("")
		return i18n.T(loc, "repl.node.cleared"), nil
	default:
		if strings.ContainsAny(arg, " \t\r\n") {
			return "", errorString(i18n.T(loc, "repl.node.usage"))
		}
		r.setNextNode(arg)
		return i18n.Tf(loc, "repl.node.set", "node", arg), nil
	}
}

// cmdNode is the classic REPL's "/node" handler.
func (r *repl) cmdNode(arg string) {
	msg, err := r.nodeCommand(arg)
	if err != nil {
		r.outln(pal().Warn(err.Error()))
		return
	}
	r.outln("  " + msg)
}

// errorString is a tiny error type for localized one-line messages, so
// nodeCommand can keep returning (string, error) like teamCommand without a
// sentinel per message.
type errorString string

func (e errorString) Error() string { return string(e) }
