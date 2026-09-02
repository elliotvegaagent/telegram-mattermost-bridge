package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/config"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/model"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/platform"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/textutil"
)

type HealthCallback func(bool)
type AcceptFunc func(context.Context, model.Event) (bool, error)

type Adapter struct {
	client       *http.Client
	methodBase   string
	fileBase     string
	chatIDs      map[string]int64
	routesByChat map[int64]string
	health       HealthCallback
	log          *slog.Logger
}

func New(client *http.Client, token, apiBase string, routes []config.Route, health HealthCallback, logger *slog.Logger) *Adapter {
	if client == nil {
		client = &http.Client{}
	}
	if health == nil {
		health = func(bool) {}
	}
	if logger == nil {
		logger = slog.Default()
	}
	chatIDs, routesByChat := map[string]int64{}, map[int64]string{}
	for _, route := range routes {
		chatIDs[route.ID], routesByChat[route.TGChatID] = route.TGChatID, route.ID
	}
	return &Adapter{
		client: client, methodBase: strings.TrimRight(apiBase, "/") + "/bot" + token,
		fileBase: strings.TrimRight(apiBase, "/") + "/file/bot" + token,
		chatIDs:  chatIDs, routesByChat: routesByChat, health: health, log: logger,
	}
}

func (a *Adapter) Platform() model.Platform { return model.Telegram }

type tgEnvelope struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

type messageEntity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

func (a *Adapter) requestForm(ctx context.Context, method string, values url.Values, timeout time.Duration, target any) error {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, a.methodBase+"/"+method, strings.NewReader(values.Encode()))
	if err != nil {
		return model.Retryable(fmt.Sprintf("Telegram request error: %T", err), 0)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return a.do(req, target)
}

func (a *Adapter) requestMultipart(ctx context.Context, method string, fields map[string]string, fieldName string, file model.MaterializedAttachment, timeout time.Duration, target any) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := w.WriteField(key, value); err != nil {
			return err
		}
	}
	part, err := w.CreateFormFile(fieldName, textutil.SafeFilename(file.FileName))
	if err != nil {
		return err
	}
	if _, err := part.Write(file.Data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, a.methodBase+"/"+method, &body)
	if err != nil {
		return model.Retryable(fmt.Sprintf("Telegram request error: %T", err), 0)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	return a.do(req, target)
}

func (a *Adapter) do(req *http.Request, target any) error {
	resp, err := a.client.Do(req)
	if err != nil {
		a.health(false)
		return model.Retryable(fmt.Sprintf("Telegram network error: %T", err), 0)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		a.health(false)
		return model.Retryable(fmt.Sprintf("Telegram response error: %T", err), 0)
	}
	var envelope tgEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		a.health(false)
		return model.Retryable("Telegram returned invalid JSON", 0)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		delay := time.Duration(envelope.Parameters.RetryAfter) * time.Second
		if delay == 0 {
			delay = 5 * time.Second
		}
		a.health(false)
		return model.Retryable("Telegram rate limit", delay)
	}
	if resp.StatusCode >= 500 {
		a.health(false)
		return model.Retryable(fmt.Sprintf("Telegram HTTP %d", resp.StatusCode), 0)
	}
	if resp.StatusCode >= 400 || !envelope.OK {
		detail := strings.ReplaceAll(envelope.Description, "\n", " ")
		if len(detail) > 300 {
			detail = detail[:300]
		}
		a.health(false)
		return model.Permanent("Telegram rejected request: " + detail)
	}
	a.health(true)
	if target != nil && len(envelope.Result) > 0 {
		if err := json.Unmarshal(envelope.Result, target); err != nil {
			return model.Retryable("Telegram result has invalid JSON", 0)
		}
	}
	return nil
}

type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
	Title     string `json:"title"`
}
type Chat struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Username string `json:"username"`
}
type File struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     *int64 `json:"file_size"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
}
type Poll struct {
	Question string `json:"question"`
}
type Message struct {
	MessageID    int64           `json:"message_id"`
	From         *User           `json:"from"`
	SenderChat   *User           `json:"sender_chat"`
	Chat         Chat            `json:"chat"`
	Date         int64           `json:"date"`
	EditDate     int64           `json:"edit_date"`
	Text         string          `json:"text"`
	Caption      string          `json:"caption"`
	MediaGroupID string          `json:"media_group_id"`
	ReplyTo      *Message        `json:"reply_to_message"`
	Photo        []File          `json:"photo"`
	Document     *File           `json:"document"`
	Video        *File           `json:"video"`
	Audio        *File           `json:"audio"`
	Voice        *File           `json:"voice"`
	Animation    *File           `json:"animation"`
	Sticker      json.RawMessage `json:"sticker"`
	Poll         *Poll           `json:"poll"`
}
type ReactionType struct {
	Type  string `json:"type"`
	Emoji string `json:"emoji"`
}
type ReactionUpdate struct {
	Chat      Chat           `json:"chat"`
	MessageID int64          `json:"message_id"`
	User      *User          `json:"user"`
	ActorChat *User          `json:"actor_chat"`
	Date      int64          `json:"date"`
	Old       []ReactionType `json:"old_reaction"`
	New       []ReactionType `json:"new_reaction"`
}
type Update struct {
	UpdateID        int64           `json:"update_id"`
	Message         *Message        `json:"message"`
	EditedMessage   *Message        `json:"edited_message"`
	MessageReaction *ReactionUpdate `json:"message_reaction"`
}

func (a *Adapter) GetMe(ctx context.Context) (User, error) {
	var result User
	err := a.requestForm(ctx, "getMe", url.Values{}, 15*time.Second, &result)
	return result, err
}
func (a *Adapter) GetChat(ctx context.Context, routeID string) (Chat, error) {
	var result Chat
	id, err := a.chatID(routeID)
	if err != nil {
		return result, err
	}
	err = a.requestForm(ctx, "getChat", url.Values{"chat_id": {strconv.FormatInt(id, 10)}}, 15*time.Second, &result)
	return result, err
}
func (a *Adapter) GetChatMember(ctx context.Context, routeID string, userID int64) (map[string]any, error) {
	var result map[string]any
	id, err := a.chatID(routeID)
	if err != nil {
		return nil, err
	}
	err = a.requestForm(ctx, "getChatMember", url.Values{"chat_id": {strconv.FormatInt(id, 10)}, "user_id": {strconv.FormatInt(userID, 10)}}, 15*time.Second, &result)
	return result, err
}
func (a *Adapter) DropPendingUpdates(ctx context.Context) error {
	return a.requestForm(ctx, "deleteWebhook", url.Values{"drop_pending_updates": {"true"}}, 15*time.Second, nil)
}

func (a *Adapter) GetUpdates(ctx context.Context, offset int64) ([]Update, error) {
	var result []Update
	err := a.requestForm(ctx, "getUpdates", url.Values{"offset": {strconv.FormatInt(offset, 10)}, "timeout": {"30"}, "allowed_updates": {`["message","edited_message","message_reaction"]`}}, 40*time.Second, &result)
	return result, err
}

func (a *Adapter) RunPolling(ctx context.Context, initialOffset, selfUserID int64, accept AcceptFunc) error {
	offset, delay := initialOffset, time.Second
	for ctx.Err() == nil {
		updates, err := a.GetUpdates(ctx, offset)
		if err != nil {
			var deliveryErr *model.DeliveryError
			wait := delay
			if ok := errorsAs(err, &deliveryErr); ok && deliveryErr.RetryAfter > 0 {
				wait = deliveryErr.RetryAfter
			}
			a.log.Warn("polling_retry", "delay_seconds", int(wait.Seconds()))
			if !sleepContext(ctx, wait) {
				return nil
			}
			delay = minDuration(60*time.Second, delay*2)
			continue
		}
		events := a.NormalizeUpdates(updates, selfUserID)
		for _, event := range events {
			if _, err := accept(ctx, event); err != nil {
				return err
			}
			if event.CheckpointValue != nil {
				value, _ := strconv.ParseInt(*event.CheckpointValue, 10, 64)
				if value > offset {
					offset = value
				}
			}
		}
		delay = time.Second
	}
	return nil
}

type groupedItem struct {
	updateID int64
	message  *Message
	kind     model.EventKind
}
type normalized struct {
	order  int64
	events []model.Event
}

func (a *Adapter) NormalizeUpdates(updates []Update, selfUserID int64) []model.Event {
	groups := [][]groupedItem{}
	albums := map[string][]groupedItem{}
	reactions := []normalized{}
	for _, update := range updates {
		if update.MessageReaction != nil {
			reactions = append(reactions, normalized{order: update.UpdateID, events: a.eventsFromReaction(update.UpdateID, update.MessageReaction, selfUserID)})
			continue
		}
		message, kind := update.Message, model.Message
		if update.EditedMessage != nil {
			message, kind = update.EditedMessage, model.Edit
		}
		if message == nil {
			groups = append(groups, []groupedItem{{updateID: update.UpdateID, kind: model.Ignored}})
			continue
		}
		item := groupedItem{updateID: update.UpdateID, message: message, kind: kind}
		if message.MediaGroupID != "" && kind == model.Message {
			albums[message.MediaGroupID] = append(albums[message.MediaGroupID], item)
		} else {
			groups = append(groups, []groupedItem{item})
		}
	}
	for _, items := range albums {
		groups = append(groups, items)
	}
	all := reactions
	for _, items := range groups {
		minID := items[0].updateID
		for _, item := range items {
			if item.updateID < minID {
				minID = item.updateID
			}
		}
		all = append(all, normalized{order: minID, events: []model.Event{a.eventFromGroup(items, selfUserID)}})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].order < all[j].order })
	result := []model.Event{}
	for _, item := range all {
		result = append(result, item.events...)
	}
	return result
}

func (a *Adapter) eventsFromReaction(updateID int64, value *ReactionUpdate, selfUserID int64) []model.Event {
	checkpoint := strconv.FormatInt(updateID+1, 10)
	routeID, routeOK := a.routesByChat[value.Chat.ID]
	sender := value.User
	if sender == nil {
		sender = value.ActorChat
	}
	if sender == nil {
		sender = &User{}
	}
	old, newValues := supportedReactions(value.Old), supportedReactions(value.New)
	type change struct {
		kind     model.EventKind
		reaction string
	}
	changes := []change{}
	for _, emoji := range old {
		if !contains(newValues, emoji) {
			changes = append(changes, change{model.ReactionRemoved, emoji})
		}
	}
	for _, emoji := range newValues {
		if !contains(old, emoji) {
			changes = append(changes, change{model.ReactionAdded, emoji})
		}
	}
	messageID := strconv.FormatInt(value.MessageID, 10)
	if !routeOK || sender.ID == selfUserID || len(changes) == 0 {
		return []model.Event{{EventID: fmt.Sprintf("tg:%d:reaction:ignored", updateID), Platform: model.Telegram, Kind: model.Ignored, MessageID: messageID, MessageIDs: []string{messageID}, AuthorID: strconv.FormatInt(sender.ID, 10), AuthorName: "ignored", RouteID: routeOrIgnored(routeID, routeOK), CheckpointKey: "telegram_offset", CheckpointValue: &checkpoint}}
	}
	result := make([]model.Event, 0, len(changes))
	for i, change := range changes {
		var checkpointValue *string
		if i == len(changes)-1 {
			checkpointValue = &checkpoint
		}
		r, _ := utf8FirstRune(change.reaction)
		result = append(result, model.Event{EventID: fmt.Sprintf("tg:%d:%s:%x", updateID, change.kind, r), Platform: model.Telegram, Kind: change.kind, MessageID: messageID, MessageIDs: []string{messageID}, AuthorID: strconv.FormatInt(sender.ID, 10), AuthorName: authorName(sender), RouteID: routeID, AuthorUsername: sender.Username, Reaction: change.reaction, CreatedAtMS: value.Date * 1000, CheckpointKey: "telegram_offset", CheckpointValue: checkpointValue})
	}
	return result
}

func (a *Adapter) eventFromGroup(items []groupedItem, selfUserID int64) model.Event {
	minID, maxID := items[0].updateID, items[0].updateID
	for _, item := range items {
		if item.updateID < minID {
			minID = item.updateID
		}
		if item.updateID > maxID {
			maxID = item.updateID
		}
	}
	checkpoint := strconv.FormatInt(maxID+1, 10)
	first := items[0].message
	if first == nil {
		id := strconv.FormatInt(minID, 10)
		return model.Event{EventID: fmt.Sprintf("tg:ignored:%d:%d", minID, maxID), Platform: model.Telegram, Kind: model.Ignored, MessageID: id, MessageIDs: []string{id}, AuthorName: "ignored", RouteID: "ignored", CheckpointKey: "telegram_offset", CheckpointValue: &checkpoint}
	}
	routeID, routeOK := a.routesByChat[first.Chat.ID]
	sender := first.From
	if sender == nil {
		sender = first.SenderChat
	}
	if sender == nil {
		sender = &User{}
	}
	supported := first.Text != "" || first.Caption != "" || len(first.Photo) > 0 || first.Document != nil || first.Video != nil || first.Audio != nil || first.Voice != nil || first.Animation != nil || len(first.Sticker) > 0 || first.Poll != nil
	if !routeOK || sender.ID == selfUserID || !supported {
		id := strconv.FormatInt(first.MessageID, 10)
		return model.Event{EventID: fmt.Sprintf("tg:ignored:%d:%d", minID, maxID), Platform: model.Telegram, Kind: model.Ignored, MessageID: id, MessageIDs: []string{id}, AuthorID: strconv.FormatInt(sender.ID, 10), AuthorName: "ignored", RouteID: routeOrIgnored(routeID, routeOK), CheckpointKey: "telegram_offset", CheckpointValue: &checkpoint}
	}
	messageIDs := []string{}
	attachments := []model.Attachment{}
	text := ""
	for _, item := range items {
		messageIDs = append(messageIDs, strconv.FormatInt(item.message.MessageID, 10))
		if text == "" {
			text = item.message.Text
			if text == "" {
				text = item.message.Caption
			}
		}
		if attachment := attachmentFromMessage(item.message); attachment != nil {
			attachments = append(attachments, *attachment)
		}
	}
	if text == "" && len(first.Sticker) > 0 {
		text = "[Стикер не поддерживается]"
	}
	if text == "" && first.Poll != nil {
		text = "[Опрос: " + first.Poll.Question + "]"
	}
	replyID, replyQuote := "", ""
	if first.ReplyTo != nil {
		replyID = strconv.FormatInt(first.ReplyTo.MessageID, 10)
		replyQuote = first.ReplyTo.Text
		if replyQuote == "" {
			replyQuote = first.ReplyTo.Caption
		}
		replyQuote = truncateRunes(replyQuote, 240)
	}
	eventID := fmt.Sprintf("tg:%d:%s", items[0].updateID, items[0].kind)
	if first.MediaGroupID != "" {
		eventID = fmt.Sprintf("tg:album:%d:%s:%d-%d", first.Chat.ID, first.MediaGroupID, minID, maxID)
	}
	return model.Event{EventID: eventID, Platform: model.Telegram, Kind: items[0].kind, MessageID: messageIDs[0], MessageIDs: messageIDs, AuthorID: strconv.FormatInt(sender.ID, 10), AuthorName: authorName(sender), RouteID: routeID, AuthorUsername: sender.Username, Text: text, ReplyToID: replyID, ReplyQuote: replyQuote, CreatedAtMS: first.Date * 1000, Attachments: attachments, CheckpointKey: "telegram_offset", CheckpointValue: &checkpoint}
}

func attachmentFromMessage(message *Message) *model.Attachment {
	id := strconv.FormatInt(message.MessageID, 10)
	if len(message.Photo) > 0 {
		photo := message.Photo[0]
		for _, candidate := range message.Photo {
			if size(candidate.FileSize) > size(photo.FileSize) {
				photo = candidate
			}
		}
		return &model.Attachment{FileID: photo.FileID, FileName: "photo_" + fallback(photo.FileUniqueID, id) + ".jpg", MimeType: "image/jpeg", Size: photo.FileSize, SourceMessageID: id}
	}
	values := []struct {
		name      string
		file      *File
		mime, ext string
	}{{"document", message.Document, "application/octet-stream", "bin"}, {"video", message.Video, "video/mp4", "mp4"}, {"audio", message.Audio, "audio/mpeg", "mp3"}, {"voice", message.Voice, "audio/ogg", "ogg"}, {"animation", message.Animation, "video/mp4", "mp4"}}
	for _, value := range values {
		if value.file == nil {
			continue
		}
		mime := fallback(value.file.MimeType, value.mime)
		name := value.file.FileName
		if name == "" {
			name = value.name + "_" + fallback(value.file.FileUniqueID, id) + "." + value.ext
		}
		return &model.Attachment{FileID: value.file.FileID, FileName: textutil.SafeFilename(name), MimeType: mime, Size: value.file.FileSize, SourceMessageID: id}
	}
	return nil
}

func (a *Adapter) FetchAuthorAvatar(ctx context.Context, event model.Event, maxBytes int64) (*model.MaterializedAttachment, error) {
	if event.Platform != model.Telegram {
		return nil, nil
	}
	userID, err := strconv.ParseInt(event.AuthorID, 10, 64)
	if err != nil {
		return nil, nil
	}
	var result struct {
		Photos [][]File `json:"photos"`
	}
	if err := a.requestForm(ctx, "getUserProfilePhotos", url.Values{"user_id": {strconv.FormatInt(userID, 10)}, "offset": {"0"}, "limit": {"1"}}, 15*time.Second, &result); err != nil {
		return nil, err
	}
	if len(result.Photos) == 0 || len(result.Photos[0]) == 0 {
		return nil, nil
	}
	photo := result.Photos[0][0]
	for _, candidate := range result.Photos[0] {
		if candidate.Width*candidate.Height < photo.Width*photo.Height {
			photo = candidate
		}
	}
	value, err := a.FetchAttachment(ctx, model.Attachment{FileID: photo.FileID, FileName: "telegram_avatar_" + event.AuthorID + ".jpg", MimeType: "image/jpeg", Size: photo.FileSize}, maxBytes)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func (a *Adapter) FetchAttachment(ctx context.Context, attachment model.Attachment, maxBytes int64) (model.MaterializedAttachment, error) {
	if attachment.Size != nil && *attachment.Size > maxBytes {
		return model.MaterializedAttachment{}, &model.AttachmentRejected{Reason: "файл " + attachment.FileName + " превышает допустимый размер"}
	}
	var info struct {
		FilePath string `json:"file_path"`
	}
	if err := a.requestForm(ctx, "getFile", url.Values{"file_id": {attachment.FileID}}, 30*time.Second, &info); err != nil {
		return model.MaterializedAttachment{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, a.fileBase+"/"+info.FilePath, nil)
	if err != nil {
		return model.MaterializedAttachment{}, model.Retryable(fmt.Sprintf("Telegram file request error: %T", err), 0)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return model.MaterializedAttachment{}, model.Retryable(fmt.Sprintf("Telegram file network error: %T", err), 0)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return model.MaterializedAttachment{}, model.Retryable(fmt.Sprintf("Telegram file HTTP %d", resp.StatusCode), 0)
	}
	if resp.StatusCode >= 400 {
		return model.MaterializedAttachment{}, model.Permanent(fmt.Sprintf("Telegram file HTTP %d", resp.StatusCode))
	}
	if resp.ContentLength > maxBytes {
		return model.MaterializedAttachment{}, &model.AttachmentRejected{Reason: "файл " + attachment.FileName + " превышает допустимый размер"}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return model.MaterializedAttachment{}, model.Retryable(fmt.Sprintf("Telegram file response error: %T", err), 0)
	}
	if int64(len(data)) > maxBytes {
		return model.MaterializedAttachment{}, &model.AttachmentRejected{Reason: "файл " + attachment.FileName + " превышает допустимый размер"}
	}
	return model.MaterializedAttachment{FileName: textutil.SafeFilename(attachment.FileName), MimeType: attachment.MimeType, Data: data, SourceMessageID: attachment.SourceMessageID}, nil
}

func (a *Adapter) Send(ctx context.Context, event model.Event, delivery model.DeliveryContext, attachments []model.MaterializedAttachment, warnings []string, completed int, onSent platform.SentCallback, avatar *model.MaterializedAttachment) error {
	chatID, err := a.chatID(event.RouteID)
	if err != nil {
		return err
	}
	text := textutil.Render(event, model.Telegram, delivery.UnknownReplyQuote, true)
	if len(warnings) > 0 {
		text += "\n\n⚠️ " + strings.Join(warnings, "; ")
	}
	type part struct {
		method, text string
		file         *model.MaterializedAttachment
	}
	parts := []part{}
	for _, chunk := range textutil.Split(text, 4096) {
		parts = append(parts, part{method: "sendMessage", text: chunk})
	}
	for i := range attachments {
		method := "sendDocument"
		if strings.HasPrefix(attachments[i].MimeType, "image/") {
			method = "sendPhoto"
		}
		parts = append(parts, part{method: method, file: &attachments[i]})
	}
	for i, item := range parts {
		if i < completed {
			continue
		}
		fields := map[string]string{"chat_id": strconv.FormatInt(chatID, 10)}
		replyTo := delivery.ReplyToID
		if replyTo == "" {
			replyTo = delivery.AnchorID
		}
		if replyTo != "" {
			payload, _ := json.Marshal(map[string]any{"message_id": mustInt64(replyTo), "allow_sending_without_reply": true})
			fields["reply_parameters"] = string(payload)
		}
		var result Message
		if item.method == "sendMessage" {
			fields["text"] = item.text
			values := url.Values{}
			for key, value := range fields {
				values.Set(key, value)
			}
			if i == 0 && event.Platform == model.Mattermost {
				addItalicAuthorEntity(values, item.text, event.AuthorName)
			}
			err = a.requestForm(ctx, item.method, values, 40*time.Second, &result)
		} else {
			fieldName := "document"
			if item.method == "sendPhoto" {
				fieldName = "photo"
			}
			err = a.requestMultipart(ctx, item.method, fields, fieldName, *item.file, 90*time.Second, &result)
		}
		if err != nil {
			return err
		}
		if err := onSent(model.SentMessage{MessageID: strconv.FormatInt(result.MessageID, 10), PartIndex: i}); err != nil {
			return err
		}
	}
	return nil
}

func (a *Adapter) SendFailureNotice(ctx context.Context, event model.Event, reason string) error {
	chatID, err := a.chatID(event.RouteID)
	if err != nil {
		return err
	}
	reason = strings.ReplaceAll(reason, "\n", " ")
	if len(reason) > 240 {
		reason = reason[:240]
	}
	values := url.Values{"chat_id": {strconv.FormatInt(chatID, 10)}, "text": {"⚠️ Сообщение не доставлено в Mattermost: " + reason}}
	if _, err := strconv.ParseInt(event.MessageID, 10, 64); err == nil {
		payload, _ := json.Marshal(map[string]any{"message_id": mustInt64(event.MessageID), "allow_sending_without_reply": true})
		values.Set("reply_parameters", string(payload))
	}
	return a.requestForm(ctx, "sendMessage", values, 20*time.Second, nil)
}

func (a *Adapter) Edit(ctx context.Context, event model.Event, targetID string) error {
	chatID, err := a.chatID(event.RouteID)
	if err != nil {
		return err
	}
	event.Kind = model.Message
	text := textutil.Render(event, model.Telegram, "", true)
	if len([]rune(text)) > 4096 {
		return model.Permanent("edited message exceeds Telegram text limit")
	}
	values := url.Values{"chat_id": {strconv.FormatInt(chatID, 10)}, "message_id": {targetID}, "text": {text}}
	if event.Platform == model.Mattermost {
		addItalicAuthorEntity(values, text, event.AuthorName)
	}
	return a.requestForm(ctx, "editMessageText", values, 20*time.Second, nil)
}
func (a *Adapter) AcknowledgeDelivery(context.Context, model.Event) error { return nil }
func (a *Adapter) SyncReactions(ctx context.Context, event model.Event, targetID string, reactions []string) error {
	chatID, err := a.chatID(event.RouteID)
	if err != nil {
		return err
	}
	if len(reactions) > 1 {
		reactions = reactions[len(reactions)-1:]
	}
	payload := []map[string]string{}
	for _, emoji := range reactions {
		payload = append(payload, map[string]string{"type": "emoji", "emoji": emoji})
	}
	raw, _ := json.Marshal(payload)
	return a.requestForm(ctx, "setMessageReaction", url.Values{"chat_id": {strconv.FormatInt(chatID, 10)}, "message_id": {targetID}, "reaction": {string(raw)}}, 20*time.Second, nil)
}

func (a *Adapter) chatID(routeID string) (int64, error) {
	id, ok := a.chatIDs[routeID]
	if !ok {
		return 0, model.Permanent("unknown bridge route: " + routeID)
	}
	return id, nil
}
func authorName(user *User) string {
	value := strings.TrimSpace(user.FirstName + " " + user.LastName)
	if value != "" {
		return value
	}
	if user.Title != "" {
		return user.Title
	}
	if user.Username != "" {
		return user.Username
	}
	return strconv.FormatInt(user.ID, 10)
}
func supportedReactions(values []ReactionType) []string {
	result := []string{}
	for _, value := range values {
		if value.Type == "emoji" {
			if _, ok := platform.MattermostByEmoji[value.Emoji]; ok {
				result = append(result, value.Emoji)
			}
		}
	}
	return result
}
func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
func routeOrIgnored(route string, ok bool) string {
	if ok {
		return route
	}
	return "ignored"
}
func fallback(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
func size(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
func truncateRunes(value string, n int) string {
	r := []rune(value)
	if len(r) <= n {
		return value
	}
	return string(r[:n])
}
func mustInt64(value string) int64 { n, _ := strconv.ParseInt(value, 10, 64); return n }
func utf8FirstRune(value string) (rune, int) {
	for _, r := range value {
		return r, len(string(r))
	}
	return 0, 0
}
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
func errorsAs(err error, target any) bool { return errors.As(err, target) }

func addItalicAuthorEntity(values url.Values, text, author string) {
	if author == "" {
		return
	}
	byteOffset := strings.Index(text, author)
	if byteOffset < 0 {
		return
	}
	afterAuthor := byteOffset + len(author)
	if afterAuthor < len(text) && text[afterAuthor] != '\n' {
		return
	}
	entities := []messageEntity{{
		Type:   "italic",
		Offset: len(utf16.Encode([]rune(text[:byteOffset]))),
		Length: len(utf16.Encode([]rune(author))),
	}}
	payload, err := json.Marshal(entities)
	if err == nil {
		values.Set("entities", string(payload))
	}
}
