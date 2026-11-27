// SPDX-License-Identifier: AGPL-3.0-or-later

package commander

import (
	"context"
	"strings"
	"sync"

	"github.com/Xustalis/OpenPanda/internal/agents"
)

type continuationKey struct{}
type continuationPrompt struct{ Full, Delta, Session, Adapter string }

// WithIncrementalPrompt keeps full recovery input available at the actual
// adapter attempt, where resume affinity (including fallback) is known.
func WithIncrementalPrompt(ctx context.Context, full, delta, session, adapter string) context.Context {
	return context.WithValue(ctx, continuationKey{}, continuationPrompt{full, delta, session, adapter})
}

func promptForAdapter(ctx context.Context, adapter, prompt string) string {
	c, ok := ctx.Value(continuationKey{}).(continuationPrompt)
	if !ok || c.Delta == "" || c.Session == "" || c.Adapter != adapter || c.Full == "" {
		return prompt
	}
	ref, ok := ctx.Value(resumeKey{}).(resumeRef)
	if !ok || ref.ID != c.Session || ref.Adapter != adapter {
		return prompt
	}
	known, ok := agents.Lookup("", adapter)
	if !ok || !known.Capabilities.SupportsIncrementalResume {
		return prompt
	}
	// Preserve a fallback identity note that the router prepended to full input.
	if !strings.HasSuffix(prompt, c.Full) {
		return prompt
	}
	return strings.TrimSuffix(prompt, c.Full) + c.Delta
}

// rejectedResume accepts a short CLI rejection for this exact checkpoint,
// before any observed tool work or new conversation. Task output containing
// a similar phrase must never trigger an automatic replay of side effects.
func rejectedResume(ctx context.Context, adapter string, result AgentResult) bool {
	ref, ok := ctx.Value(resumeKey{}).(resumeRef)
	if !ok || ref.ID == "" || ref.Adapter != adapter || result.OK || result.WorkObserved || result.SessionID != "" || (result.ExitCode != 1 && result.ExitCode != 2) {
		return false
	}
	known, ok := agents.Lookup("", adapter)
	if !ok || !known.Capabilities.SupportsIncrementalResume {
		return false
	}
	seen := false
	for _, diagnostic := range []string{result.Stderr, result.Result} {
		diagnostic = strings.TrimSpace(diagnostic)
		if diagnostic == "" {
			continue
		}
		if len(diagnostic) > 1024 || strings.ContainsAny(diagnostic, "\r\n") {
			return false
		}
		diagnostic = strings.TrimPrefix(strings.ToLower(diagnostic), "error: ")
		matched := false
		for _, prefix := range []string{"session not found: ", "no conversation found with session id: ", "conversation not found: ", "invalid session id: ", "resume session does not exist: ", "session does not exist: "} {
			if tail, found := strings.CutPrefix(diagnostic, prefix); found && strings.Trim(tail, " \"'`.") == strings.ToLower(ref.ID) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
		seen = true
	}
	return seen
}

type promptObserverKey struct{}
type PromptObserver func(adapter, mode string, bytes, fullBytes int)

func WithPromptObserver(ctx context.Context, fn PromptObserver) context.Context {
	return context.WithValue(ctx, promptObserverKey{}, fn)
}

// promptDeliveredKey carries the callback a run announces its hand-off on.
// The observer above fires before the adapter is called, which is too early to
// claim delivery — a run whose script or interpreter is missing still reaches
// it. Python adapters can acknowledge a request after parsing it, and
// native generic reports the hand-off to the configured process.
type promptDeliveredKey struct{}

type promptHandoffKey struct{}

// WithPromptHandoff runs a final check before sending a prompt to a process.
// The check may refuse a prompt whose pending user notes changed after it was built.
func WithPromptHandoff(ctx context.Context, check func() error) context.Context {
	return context.WithValue(ctx, promptHandoffKey{}, sync.OnceValue(check))
}

func beginPromptHandoff(ctx context.Context) error {
	if check, ok := ctx.Value(promptHandoffKey{}).(func() error); ok {
		return check()
	}
	return nil
}

// WithPromptDelivered registers the callback that fires once the adapter has
// taken this round's prompt. When orchestration retries a round, the caller
// may use the same callback for several adapter attempts.
func WithPromptDelivered(ctx context.Context, fn func()) context.Context {
	return context.WithValue(ctx, promptDeliveredKey{}, fn)
}

// PromptDelivered announces that the run's adapter has taken the prompt.
// Nil-safe and idempotent by convention: the registered callback owns the
// "already announced" flag.
func PromptDelivered(ctx context.Context) {
	if beginPromptHandoff(ctx) != nil {
		return
	}
	if fn, ok := ctx.Value(promptDeliveredKey{}).(func()); ok && fn != nil {
		fn()
	}
}

type nodeConfigKey struct{}

func withNodeConfigPath(ctx context.Context, path string) context.Context {
	return context.WithValue(ctx, nodeConfigKey{}, path)
}
func withNodeConfigEnv(ctx context.Context, env []string) []string {
	path, _ := ctx.Value(nodeConfigKey{}).(string)
	if path != "" {
		return append(append([]string(nil), env...), "OPENPANDA_CONFIG_PATH="+path)
	}
	return env
}
