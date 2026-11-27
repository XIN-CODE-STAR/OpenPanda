// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Xustalis/OpenPanda/internal/askengine"
	"github.com/Xustalis/OpenPanda/internal/i18n"
	"github.com/Xustalis/OpenPanda/internal/sessions"
)

// modeCommand is the single command service used by CLI and TUI.
func (r *repl) modeCommand(name, arg string) (message, prompt string, err error) {
	defer func() { r.controlMu.Lock(); r.controlAt = time.Time{}; r.controlMu.Unlock() }()
	arg = strings.TrimSpace(arg)
	if name == "goal" && arg == "" && r.sessID() == "" {
		return i18n.T(r.loc, "conversation.goalHelp"), "", nil
	}
	var planAction string
	var version int64
	if name == "plan" {
		planAction, arg, version, err = parsePlanCommand(arg)
		if err != nil {
			return "", "", err
		}
		if r.sessID() == "" && (planAction == "show" || planAction == "confirm") {
			return i18n.T(r.loc, "conversation.noProposal"), "", nil
		}
	}
	eng := r.engine.Load()
	if eng == nil {
		if name == "goal" && strings.TrimSpace(arg) == "" {
			return i18n.T(r.loc, "conversation.goalHelp"), "", nil
		}
		return "", "", errors.New("engine unavailable")
	}
	if r.sessID() == "" {
		r.startImplicitSession("/" + name)
	}
	sid := r.sessID()
	if sid == "" {
		return "", "", errors.New("session unavailable")
	}
	service := eng.Conversations(r.sessionsSt)
	ctx := context.Background()
	if planAction == "ask" {
		// The ask worker carries Mode=plan through admission. A competing
		// mode change cannot turn this command into an ordinary executable ask.
		return "", arg, nil
	}
	if planAction == "confirm" {
		_, err = service.ConfirmPlan(ctx, sid, version)
		return i18n.T(r.loc, "conversation.implementing"), "", err
	}
	state, err := service.State(ctx, sid)
	if err != nil {
		return "", "", err
	}
	var budget *int64
	if flag, rest := splitGoalWord(arg); name == "goal" && strings.EqualFold(flag, "--tokens") {
		value, text := splitGoalWord(rest)
		n, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr != nil || n <= 0 || text == "" || strings.HasPrefix(value, "+") {
			return "", "", errors.New("positive token budget required")
		}
		budget = &n
		arg = text
	}
	if name == "plan" {
		if planAction == "show" {
			if state.Proposal == "" {
				return i18n.T(r.loc, "conversation.noProposal"), "", nil
			}
			return proposalReview(r.loc, &askengine.Result{
				ProposalBody:          state.Proposal,
				ProposalVersion:       state.ProposalVersion,
				ProposalContinuesGoal: state.Goal != nil && state.Goal.PausedByPlan,
			}, ""), "", nil
		}
		if err = service.SetPlanning(ctx, sid, !state.Planning); err != nil {
			return "", "", err
		}
		if !state.Planning {
			return i18n.T(r.loc, "conversation.planOn"), "", nil
		}
		return i18n.T(r.loc, "conversation.planOff"), "", nil
	}
	if arg == "" {
		if state.Goal == nil {
			return i18n.T(r.loc, "conversation.goalHelp"), "", nil
		}
		b, _ := json.MarshalIndent(state.Goal, "", "  ")
		return string(b), "", nil
	}
	word, text := splitGoalWord(arg)
	action := strings.ToLower(word)
	if action != "edit" && text != "" && word != "--" {
		action = "create"
	}
	switch action {
	case "--":
		if text == "" {
			return i18n.T(r.loc, "conversation.goalHelp"), "", nil
		}
		_, err = service.Create(ctx, sid, text, budget)
	case "pause", "resume", "clear", "stop", "edit":
		if state.Goal == nil {
			return "", "", errors.New("no current goal")
		}
		if action == "edit" && text == "" {
			return state.Goal.Objective + "\n" + i18n.T(r.loc, "conversation.goalHelp"), "", nil
		}
		err = service.Control(ctx, sid, state.Goal.ID, action, text, budget)
	default:
		_, err = service.Create(ctx, sid, arg, budget)
	}
	if err != nil {
		return "", "", err
	}
	state, err = service.State(ctx, sid)
	b, _ := json.Marshal(state.Goal)
	return string(b), "", err
}

func parsePlanCommand(arg string) (action, prompt string, version int64, err error) {
	word, rest := splitGoalWord(arg)
	switch strings.ToLower(word) {
	case "":
		return "toggle", "", 0, nil
	case "confirm":
		version, err = strconv.ParseInt(rest, 10, 64)
		if err != nil || version <= 0 || strings.HasPrefix(rest, "+") || version > 1<<53-1 {
			return "", "", 0, errors.New("usage: /plan confirm <version>")
		}
		return "confirm", "", version, nil
	case "show":
		if rest == "" {
			return "show", "", 0, nil
		}
	case "--":
		if rest == "" {
			return "", "", 0, errors.New("usage: /plan -- <description>")
		}
		return "ask", rest, 0, nil
	}
	return "ask", arg, 0, nil
}

func splitGoalWord(text string) (word, rest string) {
	if i := strings.IndexFunc(text, unicode.IsSpace); i >= 0 {
		return text[:i], strings.TrimSpace(text[i:])
	}
	return text, ""
}

func (r *repl) conversationBadge() string {
	eng := r.engine.Load()
	sid := r.sessID()
	if eng == nil || sid == "" {
		return ""
	}
	r.controlMu.Lock()
	defer r.controlMu.Unlock()
	if r.controlSession == sid && time.Since(r.controlAt) < time.Second {
		return r.controlBadge
	}
	r.controlAt = time.Now()
	r.controlSession = sid
	r.controlBadge = ""
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	state, err := eng.Conversations(r.sessionsSt).State(ctx, sid)
	if err != nil {
		return ""
	}
	if state.Planning {
		r.controlBadge += "  [" + i18n.T(r.loc, "conversation.plan") + "]"
	}
	if state.Goal != nil {
		r.controlBadge += "  [" + i18n.T(r.loc, "conversation.goal") + " · " + i18n.T(r.loc, "conversation.status."+state.Goal.Status) + "] " + state.Goal.Objective
	}
	return r.controlBadge
}

// proposalReview belongs only to the terminal presentation. The confirmation
// command never becomes part of the stored assistant turn or executable body.
func proposalReview(loc i18n.Locale, out *askengine.Result, shown string) string {
	if out.ProposalVersion == 0 || out.ProposalBody == "" {
		return ""
	}
	key := "conversation.confirm"
	if out.ProposalContinuesGoal {
		key = "conversation.confirmGoal"
	}
	hint := fmt.Sprintf("%s /plan confirm %d", i18n.T(loc, key), out.ProposalVersion)
	if strings.Contains(shown, out.ProposalBody) {
		return hint
	}
	return out.ProposalBody + "\n\n" + hint
}

func autonomousTurn(turn sessions.Turn) bool {
	return turn.Role == "assistant" && (strings.HasPrefix(turn.IdempotencyKey, "goal:") || strings.HasPrefix(turn.IdempotencyKey, "plan:assistant:"))
}

// pollConversation delivers durable autonomous turns through the same watcher
// as task notifications. Opening a session adopts its existing transcript.
func (r *repl) pollConversation(ctx context.Context) []watchEvent {
	sid := r.sessID()
	if r.sessionsSt == nil || sid == "" || ctx.Err() != nil {
		return nil
	}
	sess, err := r.sessionsSt.Get(sid)
	if err != nil {
		return nil
	}
	r.sessMu.Lock()
	defer r.sessMu.Unlock()
	if r.activeSess != sid || r.controlTurnsSeen == nil {
		return nil
	}
	var events []watchEvent
	for _, turn := range sess.Turns {
		key := turn.IdempotencyKey
		if !autonomousTurn(turn) || turn.DisplayArchive {
			continue
		}
		if !r.controlTurnsSeen[key] && strings.TrimSpace(turn.Text) != "" {
			events = append(events, watchEvent{note: turn.Text})
		}
		r.controlTurnsSeen[key] = true
	}
	return events
}
