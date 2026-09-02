package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/config"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/model"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/platform"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/storage"
)

// Gateway is the deep module that owns durability, routing, retries and thread correlation.
// Callers only normalize external input and pass it to Accept.
type Gateway struct {
	store              *storage.Store
	adapters           map[model.Platform]platform.Adapter
	routes             map[string]config.Route
	maxAttachmentBytes int64
	log                *slog.Logger
}

func New(store *storage.Store, adapters map[model.Platform]platform.Adapter, routes []config.Route, maxAttachmentBytes int64, logger *slog.Logger) (*Gateway, error) {
	if len(routes) == 0 {
		return nil, fmt.Errorf("at least one bridge route is required")
	}
	if adapters[model.Telegram] == nil || adapters[model.Mattermost] == nil {
		return nil, fmt.Errorf("both platform adapters are required")
	}
	byID := make(map[string]config.Route, len(routes))
	for _, route := range routes {
		byID[route.ID] = route
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Gateway{store: store, adapters: adapters, routes: byID, maxAttachmentBytes: maxAttachmentBytes, log: logger}, nil
}

func (g *Gateway) Accept(ctx context.Context, event model.Event) (bool, error) {
	inserted, err := g.store.Enqueue(ctx, event)
	if err != nil {
		return false, err
	}
	g.log.Info("event_accepted", "event_id", event.EventID, "source", event.Platform, "kind", event.Kind, "duplicate", !inserted)
	return inserted, nil
}

func (g *Gateway) Run(ctx context.Context) error {
	if err := g.store.RecoverDelivering(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		job, err := g.store.FetchDueJob(ctx)
		if err != nil {
			return err
		}
		if job != nil {
			g.deliver(ctx, *job)
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (g *Gateway) deliver(ctx context.Context, job model.OutboxJob) {
	event, err := g.store.GetEvent(ctx, job.EventID)
	if err != nil {
		g.retryUnexpected(ctx, job, err)
		return
	}
	deliveryContext, err := g.resolveContext(ctx, event)
	if err != nil {
		g.handleDeliveryError(ctx, job, event, err)
		return
	}
	source, target := g.adapters[event.Platform], g.adapters[job.Target]

	if event.Kind == model.ReactionAdded || event.Kind == model.ReactionRemoved {
		targetID, err := g.reactionTargetID(ctx, event)
		if err != nil {
			g.handleDeliveryError(ctx, job, event, err)
			return
		}
		if targetID != "" {
			reactions, err := g.store.ActiveReactions(ctx, event.Platform, event.RouteID, event.MessageID)
			if err == nil && job.Target == model.Telegram && len(reactions) > 1 {
				reactions = reactions[len(reactions)-1:]
			}
			if err == nil {
				err = target.SyncReactions(ctx, event, targetID, reactions)
			}
			if err != nil {
				g.handleDeliveryError(ctx, job, event, err)
				return
			}
		}
		if err := g.store.CompleteJob(ctx, job.ID, event.EventID); err != nil {
			g.log.Error("job_complete_failed", "event_id", event.EventID, "error", errType(err))
			return
		}
		g.log.Info("reactions_synchronized", "event_id", event.EventID, "target", job.Target)
		return
	}

	if event.Kind == model.Edit && deliveryContext.EditTargetID != "" {
		if err := target.Edit(ctx, event, deliveryContext.EditTargetID); err != nil {
			g.handleDeliveryError(ctx, job, event, err)
			return
		}
		if err := g.store.CompleteJob(ctx, job.ID, event.EventID); err != nil {
			g.log.Error("job_complete_failed", "event_id", event.EventID, "error", errType(err))
			return
		}
		g.log.Info("delivery_edited", "event_id", event.EventID, "target", job.Target)
		return
	}

	var avatar *model.MaterializedAttachment
	avatarLimit := g.maxAttachmentBytes
	if avatarLimit > 128*1024 {
		avatarLimit = 128 * 1024
	}
	if value, avatarErr := source.FetchAuthorAvatar(ctx, event, avatarLimit); avatarErr == nil {
		avatar = value
	} else {
		g.log.Warn("author_avatar_unavailable", "event_id", event.EventID, "source", event.Platform, "error", errType(avatarErr))
	}

	attachments := make([]model.MaterializedAttachment, 0, len(event.Attachments))
	warnings := []string{}
	for _, item := range event.Attachments {
		value, fetchErr := source.FetchAttachment(ctx, item, g.maxAttachmentBytes)
		if fetchErr == nil {
			attachments = append(attachments, value)
			continue
		}
		var rejected *model.AttachmentRejected
		if errors.As(fetchErr, &rejected) {
			warnings = append(warnings, rejected.Error())
			continue
		}
		g.handleDeliveryError(ctx, job, event, fetchErr)
		return
	}

	completedParts := progressInt(job.Progress["completed_parts"])
	linkState := map[string]string{"mm_root": deliveryContext.RootID, "tg_anchor": deliveryContext.AnchorID}
	onSent := func(sent model.SentMessage) error {
		if err := g.recordLink(ctx, event, sent, linkState); err != nil {
			return err
		}
		job.Progress["completed_parts"] = sent.PartIndex + 1
		return g.store.UpdateProgress(ctx, job.ID, job.Progress)
	}
	if err := target.Send(ctx, event, deliveryContext, attachments, warnings, completedParts, onSent, avatar); err != nil {
		g.handleDeliveryError(ctx, job, event, err)
		return
	}
	if err := g.store.CompleteJob(ctx, job.ID, event.EventID); err != nil {
		g.log.Error("job_complete_failed", "event_id", event.EventID, "error", errType(err))
		return
	}
	if err := source.AcknowledgeDelivery(ctx, event); err != nil {
		g.log.Warn("delivery_acknowledgement_failed", "event_id", event.EventID, "source", event.Platform, "error", errType(err))
	}
	g.log.Info("delivery_completed", "event_id", event.EventID, "target", job.Target)
}

func (g *Gateway) handleDeliveryError(ctx context.Context, job model.OutboxJob, event model.Event, err error) {
	var deliveryErr *model.DeliveryError
	if errors.As(err, &deliveryErr) && !deliveryErr.Retryable {
		if storeErr := g.store.DeadLetterJob(ctx, job, deliveryErr.Error()); storeErr != nil {
			g.log.Error("dead_letter_failed", "event_id", job.EventID, "error", errType(storeErr))
			return
		}
		if noticeErr := g.adapters[event.Platform].SendFailureNotice(ctx, event, deliveryErr.Error()); noticeErr != nil {
			g.log.Error("failure_notice_failed", "event_id", job.EventID, "error", errType(noticeErr))
		}
		g.log.Error("delivery_dead_letter", "event_id", job.EventID, "target", job.Target, "error", errType(err))
		return
	}
	delay := retryDelay(job.Attempts)
	if deliveryErr != nil && deliveryErr.RetryAfter > 0 {
		delay = deliveryErr.RetryAfter
	}
	if storeErr := g.store.RetryJob(ctx, job, errType(err), delay); storeErr != nil {
		g.log.Error("retry_schedule_failed", "event_id", job.EventID, "error", errType(storeErr))
		return
	}
	g.log.Warn("delivery_retry", "event_id", job.EventID, "target", job.Target, "delay_seconds", int(delay.Seconds()), "error", errType(err))
}

func (g *Gateway) retryUnexpected(ctx context.Context, job model.OutboxJob, err error) {
	delay := retryDelay(job.Attempts)
	if storeErr := g.store.RetryJob(ctx, job, errType(err), delay); storeErr != nil {
		g.log.Error("retry_schedule_failed", "event_id", job.EventID, "error", errType(storeErr))
		return
	}
	g.log.Error("delivery_unexpected_error", "event_id", job.EventID, "target", job.Target, "error", errType(err))
}

func retryDelay(attempts int) time.Duration {
	seconds := 5 * math.Pow(2, float64(min(attempts, 6)))
	if seconds > 300 {
		seconds = 300
	}
	return time.Duration(seconds) * time.Second
}

func (g *Gateway) route(event model.Event) (config.Route, error) {
	route, ok := g.routes[event.RouteID]
	if !ok {
		return config.Route{}, model.Permanent("unknown bridge route: " + event.RouteID)
	}
	return route, nil
}

func (g *Gateway) resolveContext(ctx context.Context, event model.Event) (model.DeliveryContext, error) {
	route, err := g.route(event)
	if err != nil {
		return model.DeliveryContext{}, err
	}
	if event.Platform == model.Telegram {
		firstMessageID := event.MessageID
		if len(event.MessageIDs) > 0 {
			firstMessageID = event.MessageIDs[0]
		}
		existing, err := g.store.FindByTG(ctx, route.TGChatID, firstMessageID)
		if err != nil {
			return model.DeliveryContext{}, err
		}
		linked := existing
		if linked == nil && event.ReplyToID != "" {
			linked, err = g.store.FindByTG(ctx, route.TGChatID, event.ReplyToID)
			if err != nil {
				return model.DeliveryContext{}, err
			}
		}
		result := model.DeliveryContext{}
		if linked != nil {
			result.ReplyToID, result.RootID, result.AnchorID = linked.MMPostID, linked.MMRootID, linked.TGAnchorMessageID
		}
		if event.ReplyToID != "" && linked == nil {
			result.UnknownReplyQuote = event.ReplyQuote
		}
		if existing != nil {
			result.EditTargetID = existing.MMPostID
		}
		return result, nil
	}
	existing, err := g.store.FindByMM(ctx, event.MessageID)
	if err != nil {
		return model.DeliveryContext{}, err
	}
	linked := existing
	if linked == nil && event.RootID != "" {
		linked, err = g.store.FindByMMRoot(ctx, event.RootID)
		if err != nil {
			return model.DeliveryContext{}, err
		}
	}
	result := model.DeliveryContext{RootID: event.RootID}
	if linked != nil {
		result.ReplyToID, result.RootID, result.AnchorID = linked.TGAnchorMessageID, linked.MMRootID, linked.TGAnchorMessageID
	}
	if existing != nil {
		result.EditTargetID = existing.TGMessageID
	}
	return result, nil
}

func (g *Gateway) reactionTargetID(ctx context.Context, event model.Event) (string, error) {
	route, err := g.route(event)
	if err != nil {
		return "", err
	}
	if event.Platform == model.Telegram {
		link, err := g.store.FindByTG(ctx, route.TGChatID, event.MessageID)
		if err != nil || link == nil {
			return "", err
		}
		return link.MMPostID, nil
	}
	link, err := g.store.FindByMM(ctx, event.MessageID)
	if err != nil || link == nil {
		return "", err
	}
	return link.TGMessageID, nil
}

func (g *Gateway) recordLink(ctx context.Context, event model.Event, sent model.SentMessage, state map[string]string) error {
	route, err := g.route(event)
	if err != nil {
		return err
	}
	if event.Platform == model.Telegram {
		mmRoot := state["mm_root"]
		if mmRoot == "" {
			mmRoot = sent.RootID
		}
		if mmRoot == "" {
			mmRoot = sent.MessageID
		}
		tgAnchor := state["tg_anchor"]
		if tgAnchor == "" && len(event.MessageIDs) > 0 {
			tgAnchor = event.MessageIDs[0]
		}
		if tgAnchor == "" {
			tgAnchor = event.MessageID
		}
		state["mm_root"], state["tg_anchor"] = mmRoot, tgAnchor
		messageIDs := event.MessageIDs
		if len(messageIDs) == 0 {
			messageIDs = []string{event.MessageID}
		}
		for _, id := range messageIDs {
			if err := g.store.AddLink(ctx, model.MessageLink{MMPostID: sent.MessageID, TGChatID: route.TGChatID, TGMessageID: id, MMRootID: mmRoot, TGAnchorMessageID: tgAnchor, PartIndex: sent.PartIndex}); err != nil {
				return err
			}
		}
		return nil
	}
	mmRoot := event.RootID
	if mmRoot == "" {
		mmRoot = event.MessageID
	}
	tgAnchor := state["tg_anchor"]
	if tgAnchor == "" {
		tgAnchor = sent.MessageID
		state["tg_anchor"] = tgAnchor
	}
	return g.store.AddLink(ctx, model.MessageLink{MMPostID: event.MessageID, TGChatID: route.TGChatID, TGMessageID: sent.MessageID, MMRootID: mmRoot, TGAnchorMessageID: tgAnchor, PartIndex: sent.PartIndex})
}

func progressInt(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

func errType(err error) string {
	if err == nil {
		return ""
	}
	var deliveryErr *model.DeliveryError
	if errors.As(err, &deliveryErr) {
		if deliveryErr.Retryable {
			return "retryable_delivery_error"
		}
		return "permanent_delivery_error"
	}
	return fmt.Sprintf("%T", err)
}
