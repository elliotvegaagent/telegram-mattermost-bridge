package platform

import (
	"context"

	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/model"
)

type SentCallback func(model.SentMessage) error

type Adapter interface {
	Platform() model.Platform
	FetchAttachment(context.Context, model.Attachment, int64) (model.MaterializedAttachment, error)
	FetchAuthorAvatar(context.Context, model.Event, int64) (*model.MaterializedAttachment, error)
	Send(context.Context, model.Event, model.DeliveryContext, []model.MaterializedAttachment, []string, int, SentCallback, *model.MaterializedAttachment) error
	SendFailureNotice(context.Context, model.Event, string) error
	Edit(context.Context, model.Event, string) error
	Delete(context.Context, model.Event, []string) error
	AcknowledgeDelivery(context.Context, model.Event) error
	SyncReactions(context.Context, model.Event, string, []string) error
}

var MattermostByEmoji = map[string]string{
	"👍": "+1", "👎": "-1", "❤️": "heart", "🔥": "fire",
	"🎉": "tada", "✅": "white_check_mark", "👀": "eyes", "😂": "joy",
}

var EmojiByMattermost = func() map[string]string {
	result := map[string]string{}
	for emoji, name := range MattermostByEmoji {
		result[name] = emoji
	}
	return result
}()
