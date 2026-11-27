// SPDX-License-Identifier: AGPL-3.0-or-later

package askengine

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/Xustalis/OpenPanda/internal/conversation"
	"github.com/Xustalis/OpenPanda/internal/core"
)

// AskInput holds the result of admitting a foreground ask before its caller
// writes any turns. It can hold a clarification reservation or record that the
// ask was admitted without one.
type AskInput struct {
	store         *conversation.Store
	session       string
	wait          conversation.InputWait
	goalObjective string
	leaseCtx      context.Context
	stopLease     context.CancelFunc
	release       sync.Once
}

// ReserveAskInput validates a named clarification and reserves it before
// starting an operation. An ordinary reply takes the latest question.
func (e *Engine) ReserveAskInput(ctx context.Context, prompt string, scope AskScope) (*AskInput, error) {
	input := &AskInput{session: scope.SessionID}
	if scope.Side || scope.SessionID == "" || scope.Mode != "" || scope.confirmedImplementation || scope.goalRound || e.db == nil {
		if scope.ReplyTo != "" {
			return nil, conversation.ErrConflict
		}
		return input, nil
	}
	if scope.ReplyTo == "" {
		input.goalObjective = goalCreationObjective(prompt)
		if input.goalObjective != "" {
			return input, nil
		}
	}
	input.store = &conversation.Store{DB: e.db}
	wait, err := input.store.ReserveInput(ctx, scope.SessionID, scope.ReplyTo)
	if err != nil {
		return nil, err
	}
	input.wait = wait
	if wait.RoundID != "" {
		// An HTTP disconnect must not drop a reservation for a detached ask.
		// The caller closes it when the operation ends. Expiry handles a crash.
		input.leaseCtx, input.stopLease = context.WithCancel(context.Background())
		go input.renew()
	}
	return input, nil
}

func (r *AskInput) renew() {
	tick := time.NewTicker(core.AskRoundLease / 3)
	defer tick.Stop()
	for {
		select {
		case <-r.leaseCtx.Done():
			return
		case <-tick.C:
			if err := r.store.RenewInput(r.leaseCtx, r.wait); err != nil {
				r.stopLease()
				return
			}
		}
	}
}

func (r *AskInput) bind(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.leaseCtx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// Close releases an unconsumed answer on cancellation or any failed start.
// A successful resolution already deleted the reservation.
func (r *AskInput) Close() {
	if r == nil || r.wait.RoundID == "" {
		return
	}
	r.release.Do(func() {
		r.stopLease()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.store.ReleaseInput(ctx, r.wait)
	})
}

func goalCreationObjective(prompt string) string {
	text := strings.TrimSpace(prompt)
	lower := strings.ToLower(text)
	for _, prefix := range []string{"/goal ", "create goal: ", "创建目标："} {
		if strings.HasPrefix(lower, prefix) {
			return strings.TrimSpace(text[len(prefix):])
		}
	}
	return ""
}
