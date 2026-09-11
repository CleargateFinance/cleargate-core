package account

import (
	"context"
	"strings"
	"unicode/utf8"
)

// Bounds on what an agent may record about a run. Both are written by an
// authenticated caller on a hot path, so neither is unbounded.
const (
	// MaxTaskRefLength fits any identifier a caller's own system would use.
	MaxTaskRefLength = 256

	// MaxTaskTextLength fits a description a human will actually read during a
	// dispute, rather than a transcript.
	MaxTaskTextLength = 4096
)

// StartAgentSessionParams describes a run about to begin.
type StartAgentSessionParams struct {
	// TaskRef is the caller's own identifier for this run, so a payment can be
	// matched back to a job in their system later.
	TaskRef string

	// TaskText is what the agent was asked to do, in words. It is the evidence
	// a human reads when deciding whether a payment should have happened, so it
	// is worth more than an opaque identifier and both are recorded.
	TaskText string
}

// StartAgentSession opens a run for an already-authenticated agent.
//
// Opened explicitly rather than inferred, because what makes a run useful is
// knowing which payments belonged to the same piece of work, and no gap between
// timestamps establishes that afterwards. An agent that never opens one still
// works, it simply has payments that belong to no run.
//
// The identity comes from a verified credential rather than an identifier the
// caller supplies, so an agent cannot open a run against somebody else.
func (s *Service) StartAgentSession(ctx context.Context, identity AgentIdentity, p StartAgentSessionParams) (AgentSession, error) {
	taskRef := strings.TrimSpace(p.TaskRef)
	taskText := strings.TrimSpace(p.TaskText)

	if utf8.RuneCountInString(taskRef) > MaxTaskRefLength {
		return AgentSession{}, errInvalid("task reference is too long")
	}
	if utf8.RuneCountInString(taskText) > MaxTaskTextLength {
		return AgentSession{}, errInvalid("task description is too long")
	}

	return s.repo.CreateAgentSession(ctx, identity.AgentID, taskRef, taskText)
}
