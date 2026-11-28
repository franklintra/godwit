package core

import (
	"context"
	"slices"
)

// OpenedThread is what the reader needs to show a thread it opens: the
// thread, its labels, the message it opens with that message's body, and
// whether the message's remote images load without asking. It is what
// GetThread, ThreadLabels, GetMessageLazyImages and ImagesAllowed return,
// read in one call so the reader waits for one answer before it paints.
type OpenedThread struct {
	Thread Thread
	Labels []AppliedLabel
	// MessageID is the message the reader opens: the one the route names
	// (the message it was merged into, ADR 0034), else the thread's
	// newest. It is 0 when the thread shows no such message: Message is
	// then empty.
	MessageID int64
	// Message is MessageID read as GetMessageLazyImages(MessageID, false)
	// reads it: remote and inline images are left for its page to ask.
	Message       Message
	ImagesAllowed bool // ImagesAllowed(MessageID)
}

// OpenThread reads thread threadID as the reader opens it, messageID being
// the message the route names (0 for none). It only combines the reads the
// reader made one by one before, so each part equals what that read gives.
func (s *Service) OpenThread(ctx context.Context, threadID, messageID int64) (OpenedThread, error) {
	ctx, end, err := s.begin(ctx, 0)
	if err != nil {
		return OpenedThread{}, err
	}
	defer end()
	th, err := s.GetThread(ctx, threadID)
	if err != nil {
		return OpenedThread{}, err
	}
	out := OpenedThread{Thread: th}
	inThread := func(id int64) bool {
		return slices.ContainsFunc(th.Messages, func(m MessageSummary) bool { return m.ID == id })
	}
	if messageID != 0 && !inThread(messageID) {
		m, err := s.GetMessageLazyImages(ctx, messageID, false)
		if err != nil {
			return OpenedThread{}, err
		}
		messageID = m.ID // merged into another message since the route was set
	}
	switch {
	case messageID != 0 && inThread(messageID):
		out.MessageID = messageID
	case messageID == 0 && len(th.Messages) > 0:
		out.MessageID = th.Messages[len(th.Messages)-1].ID
	}
	if out.MessageID != 0 {
		if out.Message, err = s.GetMessageLazyImages(ctx, out.MessageID, false); err != nil {
			return OpenedThread{}, err
		}
		if out.ImagesAllowed, err = s.ImagesAllowed(ctx, out.MessageID); err != nil {
			return OpenedThread{}, err
		}
	}
	labels, err := s.ThreadLabels(ctx, []int64{threadID})
	if err != nil {
		return OpenedThread{}, err
	}
	out.Labels = labels[threadID]
	return out, nil
}
