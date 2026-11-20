package core

import (
	"context"
	"log/slog"

	"github.com/chyroc/ama/server/model/persistmodel"
)

// FeedbackStore is the narrow persistence contract for problem feedback.
// Automatic deduplication is implemented by the store's unique constraint, not this service.
type FeedbackStore interface {
	PutFeedback(ctx context.Context, rec persistmodel.FeedbackRecord) error
	ListFeedback(ctx context.Context, q persistmodel.FeedbackQuery) ([]persistmodel.FeedbackRecord, int, error)
}

// FeedbackService owns problem-feedback collection and staff-only reads.
// readers is deliberately evaluated on every ListFeedback call; a nil reader denies everyone.
type FeedbackService struct {
	store   FeedbackStore
	nextID  func(prefix string) string
	readers func() FeedbackReaders
	log     *slog.Logger
}

// NewFeedbackService builds the service with only its own dependencies.
// nextID is required for SubmitFeedback. readers may be nil to disable all reads.
func NewFeedbackService(store FeedbackStore, nextID func(string) string, readers FeedbackReaders, logger *slog.Logger) *FeedbackService {
	if logger == nil {
		logger = slog.Default()
	}
	return &FeedbackService{store: store, nextID: nextID, readers: func() FeedbackReaders { return readers }, log: logger}
}

// NewFeedbackServiceWithReader builds a service whose reader is resolved on every read.
func NewFeedbackServiceWithReader(
	store FeedbackStore, nextID func(string) string,
	readers func() FeedbackReaders, logger *slog.Logger,
) *FeedbackService {
	if logger == nil {
		logger = slog.Default()
	}
	return &FeedbackService{store: store, nextID: nextID, readers: readers, log: logger}
}

func (s *FeedbackService) logWarn(ctx context.Context, msg string, args ...any) {
	s.log.WarnContext(ctx, msg, args...)
}
