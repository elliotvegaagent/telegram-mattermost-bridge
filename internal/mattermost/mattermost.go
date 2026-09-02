package mattermost

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/config"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/model"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/platform"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/textutil"
)

type HealthCallback func(bool)
type AcceptFunc func(context.Context, model.Event) (bool, error)

type Adapter struct {
	client          *http.Client
	baseURL         string
	apiBase         string
	token           string
	channelIDs      map[string]string
	routesByChannel map[string]string
	health          HealthCallback
	log             *slog.Logger
	mu              sync.Mutex
	users           map[string]User
	selfUserID      string
	clientConfig    map[string]string
}

func New(client *http.Client, baseURL, token string, routes []config.Route, health HealthCallback, logger *slog.Logger) *Adapter {
	if client == nil {
		client = &http.Client{}
	}
	if health == nil {
		health = func(bool) {}
	}
	if logger == nil {
		logger = slog.Default()
	}
	channelIDs, routesByChannel := map[string]string{}, map[string]string{}
	for _, route := range routes {
		channelIDs[route.ID] = route.MMChannelID
		routesByChannel[route.MMChannelID] = route.ID
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &Adapter{client: client, baseURL: baseURL, apiBase: baseURL + "/api/v4", token: token, channelIDs: channelIDs, routesByChannel: routesByChannel, health: health, log: logger, users: map[string]User{}}
}
func (a *Adapter) Platform() model.Platform { return model.Mattermost }

func (a *Adapter) request(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string, timeout time.Duration, target any) error {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	endpoint := a.apiBase + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(requestCtx, method, endpoint, body)
	if err != nil {
		return model.Retryable(fmt.Sprintf("Mattermost request error: %T", err), 0)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		a.health(false)
		return model.Retryable(fmt.Sprintf("Mattermost network error: %T", err), 0)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		delay := 5 * time.Second
		if value, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && value > 0 {
			delay = time.Duration(value) * time.Second
		}
		a.health(false)
		return model.Retryable("Mattermost rate limit", delay)
	}
	if resp.StatusCode >= 500 {
		a.health(false)
		return model.Retryable(fmt.Sprintf("Mattermost HTTP %d", resp.StatusCode), 0)
	}
	if resp.StatusCode >= 400 {
		a.health(false)
		return model.Permanent(fmt.Sprintf("Mattermost HTTP %d", resp.StatusCode))
	}
	if target != nil {
		if bytesTarget, ok := target.(*[]byte); ok {
			value, err := io.ReadAll(resp.Body)
			if err != nil {
				return model.Retryable(fmt.Sprintf("Mattermost response error: %T", err), 0)
			}
			*bytesTarget = value
		} else if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(target); err != nil {
			return model.Retryable("Mattermost returned invalid JSON", 0)
		}
	}
	a.health(true)
	return nil
}
func (a *Adapter) jsonRequest(ctx context.Context, method, path string, query url.Values, value, target any, timeout time.Duration) error {
	var body io.Reader
	if value != nil {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	return a.request(ctx, method, path, query, body, "application/json", timeout, target)
}

type User struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Nickname  string `json:"nickname"`
	IsBot     bool   `json:"is_bot"`
}
type Channel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}
type FileInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mime_type"`
	Size     *int64 `json:"size"`
}
type PostMetadata struct {
	Files []FileInfo `json:"files"`
}
type Post struct {
	ID        string       `json:"id"`
	ChannelID string       `json:"channel_id"`
	UserID    string       `json:"user_id"`
	RootID    string       `json:"root_id"`
	Message   string       `json:"message"`
	Type      string       `json:"type"`
	CreateAt  int64        `json:"create_at"`
	UpdateAt  int64        `json:"update_at"`
	EditAt    int64        `json:"edit_at"`
	DeleteAt  int64        `json:"delete_at"`
	FileIDs   []string     `json:"file_ids"`
	Metadata  PostMetadata `json:"metadata"`
}
type Reaction struct {
	UserID    string `json:"user_id"`
	PostID    string `json:"post_id"`
	EmojiName string `json:"emoji_name"`
	CreateAt  int64  `json:"create_at"`
}

func (a *Adapter) GetMe(ctx context.Context) (User, error) {
	var result User
	err := a.jsonRequest(ctx, http.MethodGet, "/users/me", nil, nil, &result, 20*time.Second)
	if err == nil {
		a.mu.Lock()
		a.selfUserID = result.ID
		a.mu.Unlock()
	}
	return result, err
}
func (a *Adapter) GetChannel(ctx context.Context, routeID string) (Channel, error) {
	var result Channel
	id, err := a.channelID(routeID)
	if err != nil {
		return result, err
	}
	err = a.jsonRequest(ctx, http.MethodGet, "/channels/"+id, nil, nil, &result, 20*time.Second)
	return result, err
}
func (a *Adapter) GetChannelMember(ctx context.Context, routeID, userID string) (map[string]any, error) {
	var result map[string]any
	id, err := a.channelID(routeID)
	if err != nil {
		return nil, err
	}
	err = a.jsonRequest(ctx, http.MethodGet, "/channels/"+id+"/members/"+userID, nil, nil, &result, 20*time.Second)
	return result, err
}
func (a *Adapter) GetClientConfig(ctx context.Context) (map[string]string, error) {
	a.mu.Lock()
	if a.clientConfig != nil {
		value := a.clientConfig
		a.mu.Unlock()
		return value, nil
	}
	a.mu.Unlock()
	var result map[string]string
	err := a.jsonRequest(ctx, http.MethodGet, "/config/client", url.Values{"format": {"old"}}, nil, &result, 20*time.Second)
	if err == nil {
		a.mu.Lock()
		a.clientConfig = result
		a.mu.Unlock()
	}
	return result, err
}
func (a *Adapter) GetUser(ctx context.Context, id string) (User, error) {
	a.mu.Lock()
	value, ok := a.users[id]
	a.mu.Unlock()
	if ok {
		return value, nil
	}
	var result User
	err := a.jsonRequest(ctx, http.MethodGet, "/users/"+id, nil, nil, &result, 20*time.Second)
	if err == nil {
		a.mu.Lock()
		a.users[id] = result
		a.mu.Unlock()
	}
	return result, err
}
func (a *Adapter) GetPostFiles(ctx context.Context, id string) ([]FileInfo, error) {
	var result []FileInfo
	err := a.jsonRequest(ctx, http.MethodGet, "/posts/"+id+"/files/info", nil, nil, &result, 30*time.Second)
	return result, err
}

func (a *Adapter) CatchUp(ctx context.Context, checkpoints map[string]int64) ([]Post, error) {
	byID := map[string]Post{}
	for routeID, channelID := range a.channelIDs {
		since := checkpoints[routeID]
		var result struct {
			Posts map[string]Post `json:"posts"`
		}
		if err := a.jsonRequest(ctx, http.MethodGet, "/channels/"+channelID+"/posts", url.Values{"since": {strconv.FormatInt(max64(0, since), 10)}}, nil, &result, 60*time.Second); err != nil {
			return nil, err
		}
		for id, post := range result.Posts {
			byID[id] = post
		}
	}
	posts := make([]Post, 0, len(byID))
	for _, post := range byID {
		posts = append(posts, post)
	}
	sort.Slice(posts, func(i, j int) bool {
		if posts[i].CreateAt == posts[j].CreateAt {
			return posts[i].ID < posts[j].ID
		}
		return posts[i].CreateAt < posts[j].CreateAt
	})
	return posts, nil
}

func (a *Adapter) EventFromPost(ctx context.Context, post Post, kind model.EventKind, selfUserID string) (model.Event, error) {
	checkpoint := max64(post.CreateAt, max64(post.UpdateAt, post.DeleteAt))
	routeID, routeOK := a.routesByChannel[post.ChannelID]
	ignored := !routeOK || post.UserID == selfUserID || post.Type != ""
	checkpointValue := strconv.FormatInt(checkpoint, 10)
	if ignored {
		return model.Event{EventID: fmt.Sprintf("mm:%s:ignored:%d", fallback(post.ID, strconv.FormatInt(checkpoint, 10)), checkpoint), Platform: model.Mattermost, Kind: model.Ignored, MessageID: fallback(post.ID, strconv.FormatInt(checkpoint, 10)), MessageIDs: []string{fallback(post.ID, strconv.FormatInt(checkpoint, 10))}, AuthorID: post.UserID, AuthorName: "ignored", RouteID: routeOrIgnored(routeID, routeOK), CheckpointKey: checkpointKey(routeID, routeOK), CheckpointValue: valueOrNil(checkpointValue, routeOK)}, nil
	}
	user, err := a.GetUser(ctx, post.UserID)
	if err != nil {
		user = User{ID: post.UserID, Username: post.UserID}
	}
	if user.IsBot {
		return model.Event{EventID: fmt.Sprintf("mm:%s:ignored-bot:%d", post.ID, checkpoint), Platform: model.Mattermost, Kind: model.Ignored, MessageID: post.ID, MessageIDs: []string{post.ID}, AuthorID: post.UserID, AuthorName: "ignored", RouteID: routeID, CheckpointKey: "mattermost_checkpoint_ms:" + routeID, CheckpointValue: &checkpointValue}, nil
	}
	files := post.Metadata.Files
	if len(files) == 0 && len(post.FileIDs) > 0 {
		if fetched, fetchErr := a.GetPostFiles(ctx, post.ID); fetchErr == nil {
			files = fetched
		}
	}
	attachments := []model.Attachment{}
	if kind == model.Message {
		for _, info := range files {
			if info.ID == "" {
				continue
			}
			mime := fallback(info.MimeType, "application/octet-stream")
			attachments = append(attachments, model.Attachment{FileID: info.ID, FileName: textutil.SafeFilename(fallback(info.Name, "attachment")), MimeType: mime, Size: info.Size, SourceMessageID: post.ID})
		}
	}
	suffix := "ignored"
	switch kind {
	case model.Message:
		suffix = "message"
	case model.Edit:
		suffix = fmt.Sprintf("edit:%d", max64(post.EditAt, post.UpdateAt))
	case model.Delete:
		suffix = fmt.Sprintf("delete:%d", max64(post.DeleteAt, post.UpdateAt))
	}
	return model.Event{EventID: "mm:" + post.ID + ":" + suffix, Platform: model.Mattermost, Kind: kind, MessageID: post.ID, MessageIDs: []string{post.ID}, AuthorID: post.UserID, AuthorName: mmAuthorName(user), RouteID: routeID, AuthorUsername: user.Username, Text: post.Message, RootID: post.RootID, CreatedAtMS: post.CreateAt, Attachments: attachments, CheckpointKey: "mattermost_checkpoint_ms:" + routeID, CheckpointValue: &checkpointValue}, nil
}

func (a *Adapter) EventFromReaction(reaction Reaction, kind model.EventKind, channelID, selfUserID string) model.Event {
	routeID, routeOK := a.routesByChannel[channelID]
	canonical, emojiOK := platform.EmojiByMattermost[reaction.EmojiName]
	ignored := !routeOK || reaction.UserID == selfUserID || reaction.PostID == "" || !emojiOK
	suffix := string(kind)
	if ignored {
		suffix = "ignored"
	}
	return model.Event{EventID: fmt.Sprintf("mm:%s:%s:%s:%s:%d", fallback(reaction.PostID, "unknown"), suffix, reaction.UserID, reaction.EmojiName, reaction.CreateAt), Platform: model.Mattermost, Kind: kindOrIgnored(kind, ignored), MessageID: fallback(reaction.PostID, "unknown"), MessageIDs: []string{fallback(reaction.PostID, "unknown")}, AuthorID: reaction.UserID, AuthorName: nameOrIgnored(reaction.UserID, ignored), RouteID: routeOrIgnored(routeID, routeOK), Reaction: canonical, CreatedAtMS: reaction.CreateAt}
}

func (a *Adapter) RunWebSocket(ctx context.Context, initial map[string]int64, selfUserID string, accept AcceptFunc) error {
	checkpoints := map[string]int64{}
	for id, value := range initial {
		checkpoints[id] = value
	}
	first := true
	delay := time.Second
	for ctx.Err() == nil {
		since := map[string]int64{}
		for id, value := range checkpoints {
			if !first {
				value = max64(0, value-5*60*1000)
			}
			since[id] = value
		}
		posts, err := a.CatchUp(ctx, since)
		if err == nil {
			for _, post := range posts {
				kind := kindFromPost(post)
				event, eventErr := a.EventFromPost(ctx, post, kind, selfUserID)
				if eventErr != nil {
					return eventErr
				}
				if _, eventErr = accept(ctx, event); eventErr != nil {
					return eventErr
				}
				updateCheckpoint(checkpoints, event)
			}
			first = false
			err = a.consumeSocket(ctx, checkpoints, selfUserID, accept)
		}
		if ctx.Err() != nil {
			return nil
		}
		a.health(false)
		a.log.Warn("websocket_retry", "delay_seconds", int(delay.Seconds()))
		if !sleepContext(ctx, delay) {
			return nil
		}
		delay = minDuration(60*time.Second, delay*2)
	}
	return nil
}

type wsEnvelope struct {
	Event     string                     `json:"event"`
	Data      map[string]json.RawMessage `json:"data"`
	Broadcast struct {
		ChannelID string `json:"channel_id"`
	} `json:"broadcast"`
}

func (a *Adapter) consumeSocket(ctx context.Context, checkpoints map[string]int64, selfUserID string, accept AcceptFunc) error {
	parsed, err := url.Parse(a.baseURL)
	if err != nil {
		return err
	}
	scheme := "ws"
	if parsed.Scheme == "https" {
		scheme = "wss"
	}
	wsURL := scheme + "://" + parsed.Host + "/api/v4/websocket"
	dialer := websocket.Dialer{HandshakeTimeout: 20 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, wsURL, nil)
	if resp != nil {
		resp.Body.Close()
	}
	if err != nil {
		return model.Retryable(fmt.Sprintf("Mattermost WebSocket error: %T", err), 0)
	}
	defer conn.Close()
	closed := make(chan struct{})
	defer close(closed)
	go func() {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = conn.Close()
				return
			case <-closed:
				return
			case <-ticker.C:
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			}
		}
	}()
	auth := map[string]any{"seq": 1, "action": "authentication_challenge", "data": map[string]string{"token": a.token}}
	if err := conn.WriteJSON(auth); err != nil {
		return model.Retryable("Mattermost WebSocket authentication failed", 0)
	}
	a.health(true)
	conn.SetReadDeadline(time.Now().Add(40 * time.Second))
	conn.SetPongHandler(func(string) error { conn.SetReadDeadline(time.Now().Add(40 * time.Second)); a.health(true); return nil })
	for ctx.Err() == nil {
		var payload wsEnvelope
		if err := conn.ReadJSON(&payload); err != nil {
			return model.Retryable("Mattermost WebSocket closed", 0)
		}
		conn.SetReadDeadline(time.Now().Add(40 * time.Second))
		switch payload.Event {
		case "posted", "post_edited", "post_deleted":
			var post Post
			if !decodePossiblyString(payload.Data["post"], &post) {
				continue
			}
			kind := map[string]model.EventKind{"posted": model.Message, "post_edited": model.Edit, "post_deleted": model.Delete}[payload.Event]
			event, eventErr := a.EventFromPost(ctx, post, kind, selfUserID)
			if eventErr != nil {
				return eventErr
			}
			if _, eventErr = accept(ctx, event); eventErr != nil {
				return eventErr
			}
			updateCheckpoint(checkpoints, event)
		case "reaction_added", "reaction_removed":
			var reaction Reaction
			if !decodePossiblyString(payload.Data["reaction"], &reaction) {
				continue
			}
			channelID := payload.Broadcast.ChannelID
			if channelID == "" {
				_ = json.Unmarshal(payload.Data["channel_id"], &channelID)
			}
			kind := model.ReactionAdded
			if payload.Event == "reaction_removed" {
				kind = model.ReactionRemoved
			}
			if _, err := accept(ctx, a.EventFromReaction(reaction, kind, channelID, selfUserID)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *Adapter) FetchAttachment(ctx context.Context, attachment model.Attachment, maxBytes int64) (model.MaterializedAttachment, error) {
	if attachment.Size != nil && *attachment.Size > maxBytes {
		return model.MaterializedAttachment{}, &model.AttachmentRejected{Reason: "файл " + attachment.FileName + " превышает допустимый размер"}
	}
	requestCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, a.apiBase+"/files/"+attachment.FileID, nil)
	if err != nil {
		return model.MaterializedAttachment{}, model.Retryable(fmt.Sprintf("Mattermost file request error: %T", err), 0)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := a.client.Do(req)
	if err != nil {
		return model.MaterializedAttachment{}, model.Retryable(fmt.Sprintf("Mattermost file network error: %T", err), 0)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return model.MaterializedAttachment{}, model.Retryable(fmt.Sprintf("Mattermost file HTTP %d", resp.StatusCode), 0)
	}
	if resp.StatusCode >= 400 {
		return model.MaterializedAttachment{}, model.Permanent(fmt.Sprintf("Mattermost file HTTP %d", resp.StatusCode))
	}
	if resp.ContentLength > maxBytes {
		return model.MaterializedAttachment{}, &model.AttachmentRejected{Reason: "файл " + attachment.FileName + " превышает допустимый размер"}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return model.MaterializedAttachment{}, model.Retryable(fmt.Sprintf("Mattermost file response error: %T", err), 0)
	}
	if int64(len(data)) > maxBytes {
		return model.MaterializedAttachment{}, &model.AttachmentRejected{Reason: "файл " + attachment.FileName + " превышает допустимый размер"}
	}
	return model.MaterializedAttachment{FileName: textutil.SafeFilename(attachment.FileName), MimeType: attachment.MimeType, Data: data, SourceMessageID: attachment.SourceMessageID}, nil
}
func (a *Adapter) FetchAuthorAvatar(context.Context, model.Event, int64) (*model.MaterializedAttachment, error) {
	return nil, nil
}

func (a *Adapter) uploadFiles(ctx context.Context, files []model.MaterializedAttachment, channelID string) ([]string, error) {
	if len(files) == 0 {
		return nil, nil
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("channel_id", channelID); err != nil {
		return nil, err
	}
	for _, file := range files {
		header := make(map[string][]string)
		header["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name="files"; filename="%s"`, strings.ReplaceAll(textutil.SafeFilename(file.FileName), `"`, `'`))}
		header["Content-Type"] = []string{fallback(file.MimeType, "application/octet-stream")}
		part, err := w.CreatePart(textproto.MIMEHeader(header))
		if err != nil {
			return nil, err
		}
		if _, err = part.Write(file.Data); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	var result struct {
		FileInfos []FileInfo `json:"file_infos"`
	}
	if err := a.request(ctx, http.MethodPost, "/files", nil, &body, w.FormDataContentType(), 120*time.Second, &result); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(result.FileInfos))
	for _, info := range result.FileInfos {
		ids = append(ids, info.ID)
	}
	return ids, nil
}

func (a *Adapter) Send(ctx context.Context, event model.Event, delivery model.DeliveryContext, attachments []model.MaterializedAttachment, warnings []string, completed int, onSent platform.SentCallback, avatar *model.MaterializedAttachment) error {
	channelID, err := a.channelID(event.RouteID)
	if err != nil {
		return err
	}
	props := map[string]any{"from_telegram_mattermost_bridge": true}
	if event.Platform == model.Telegram {
		cfg, cfgErr := a.GetClientConfig(ctx)
		if cfgErr != nil {
			return cfgErr
		}
		if strings.EqualFold(cfg["EnablePostUsernameOverride"], "true") {
			props["override_username"] = textutil.AuthorLabel(event)
		}
		if strings.EqualFold(cfg["EnablePostIconOverride"], "true") && avatar != nil && strings.HasPrefix(avatar.MimeType, "image/") {
			props["override_icon_url"] = "data:" + avatar.MimeType + ";base64," + base64.StdEncoding.EncodeToString(avatar.Data)
		}
	}
	text := textutil.Render(event, model.Mattermost, delivery.UnknownReplyQuote, true)
	if len(warnings) > 0 {
		text += "\n\n⚠️ " + strings.Join(warnings, "; ")
	}
	chunks := textutil.Split(text, 16000)
	fileGroups := [][]model.MaterializedAttachment{}
	for i := 0; i < len(attachments); i += 5 {
		end := minInt(i+5, len(attachments))
		fileGroups = append(fileGroups, attachments[i:end])
	}
	type part struct {
		text  string
		files []model.MaterializedAttachment
	}
	parts := []part{}
	if len(fileGroups) > 0 {
		parts = append(parts, part{chunks[0], fileGroups[0]})
		for _, group := range fileGroups[1:] {
			parts = append(parts, part{"📎 Вложения (продолжение)", group})
		}
		for _, chunk := range chunks[1:] {
			parts = append(parts, part{text: chunk})
		}
	} else {
		for _, chunk := range chunks {
			parts = append(parts, part{text: chunk})
		}
	}
	rootID := delivery.RootID
	for i, item := range parts {
		if i < completed {
			continue
		}
		fileIDs, err := a.uploadFiles(ctx, item.files, channelID)
		if err != nil {
			return err
		}
		body := map[string]any{"channel_id": channelID, "message": item.text, "props": props}
		if rootID != "" {
			body["root_id"] = rootID
		}
		if len(fileIDs) > 0 {
			body["file_ids"] = fileIDs
		}
		var result Post
		if err := a.jsonRequest(ctx, http.MethodPost, "/posts", nil, body, &result, 60*time.Second); err != nil {
			return err
		}
		if rootID == "" {
			rootID = result.ID
		}
		if err := onSent(model.SentMessage{MessageID: result.ID, RootID: rootID, PartIndex: i}); err != nil {
			return err
		}
	}
	return nil
}

func (a *Adapter) SendFailureNotice(ctx context.Context, event model.Event, reason string) error {
	channelID, err := a.channelID(event.RouteID)
	if err != nil {
		return err
	}
	reason = strings.ReplaceAll(reason, "\n", " ")
	if len(reason) > 240 {
		reason = reason[:240]
	}
	rootID := event.RootID
	if rootID == "" {
		rootID = event.MessageID
	}
	return a.jsonRequest(ctx, http.MethodPost, "/posts", nil, map[string]any{"channel_id": channelID, "root_id": rootID, "message": "⚠️ Сообщение не доставлено в Telegram: " + reason, "props": map[string]any{"from_telegram_mattermost_bridge": true}}, nil, 30*time.Second)
}
func (a *Adapter) Edit(ctx context.Context, event model.Event, targetID string) error {
	event.Kind = model.Message
	message := textutil.Render(event, model.Mattermost, "", true)
	return a.jsonRequest(ctx, http.MethodPut, "/posts/"+targetID+"/patch", nil, map[string]string{"message": message}, nil, 30*time.Second)
}
func (a *Adapter) Delete(ctx context.Context, _ model.Event, targetIDs []string) error {
	for _, targetID := range targetIDs {
		if err := a.jsonRequest(ctx, http.MethodDelete, "/posts/"+targetID, nil, nil, nil, 30*time.Second); err != nil {
			return err
		}
	}
	return nil
}
func (a *Adapter) AcknowledgeDelivery(context.Context, model.Event) error { return nil }
func (a *Adapter) SyncReactions(ctx context.Context, event model.Event, targetID string, reactions []string) error {
	a.mu.Lock()
	selfID := a.selfUserID
	a.mu.Unlock()
	if selfID == "" {
		me, err := a.GetMe(ctx)
		if err != nil {
			return err
		}
		selfID = me.ID
	}
	var currentPayload []Reaction
	if err := a.jsonRequest(ctx, http.MethodGet, "/posts/"+targetID+"/reactions", nil, nil, &currentPayload, 30*time.Second); err != nil {
		return err
	}
	managed := map[string]bool{}
	for _, name := range platform.MattermostByEmoji {
		managed[name] = true
	}
	current := map[string]bool{}
	for _, item := range currentPayload {
		if item.UserID == selfID && managed[item.EmojiName] {
			current[item.EmojiName] = true
		}
	}
	desired := map[string]bool{}
	for _, reaction := range reactions {
		if name, ok := platform.MattermostByEmoji[reaction]; ok {
			desired[name] = true
		}
	}
	for name := range desired {
		if !current[name] {
			if err := a.jsonRequest(ctx, http.MethodPost, "/reactions", nil, Reaction{UserID: selfID, PostID: targetID, EmojiName: name}, nil, 30*time.Second); err != nil {
				return err
			}
		}
	}
	for name := range current {
		if !desired[name] {
			if err := a.jsonRequest(ctx, http.MethodDelete, "/users/"+selfID+"/posts/"+targetID+"/reactions/"+url.PathEscape(name), nil, nil, nil, 30*time.Second); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *Adapter) channelID(routeID string) (string, error) {
	id, ok := a.channelIDs[routeID]
	if !ok {
		return "", model.Permanent("unknown bridge route: " + routeID)
	}
	return id, nil
}
func kindFromPost(post Post) model.EventKind {
	if post.DeleteAt > 0 {
		return model.Delete
	}
	if post.EditAt > post.CreateAt {
		return model.Edit
	}
	return model.Message
}
func updateCheckpoint(checkpoints map[string]int64, event model.Event) {
	if event.CheckpointValue == nil {
		return
	}
	value, err := strconv.ParseInt(*event.CheckpointValue, 10, 64)
	if err == nil && value > checkpoints[event.RouteID] {
		checkpoints[event.RouteID] = value
	}
}
func decodePossiblyString(raw json.RawMessage, target any) bool {
	if len(raw) == 0 {
		return false
	}
	if raw[0] == '"' {
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return false
		}
		return json.Unmarshal([]byte(value), target) == nil
	}
	return json.Unmarshal(raw, target) == nil
}
func mmAuthorName(user User) string {
	value := strings.TrimSpace(user.FirstName + " " + user.LastName)
	if value != "" {
		return value
	}
	if user.Nickname != "" {
		return user.Nickname
	}
	if user.Username != "" {
		return user.Username
	}
	return user.ID
}
func fallback(value, other string) string {
	if value != "" {
		return value
	}
	return other
}
func routeOrIgnored(route string, ok bool) string {
	if ok {
		return route
	}
	return "ignored"
}
func checkpointKey(route string, ok bool) string {
	if ok {
		return "mattermost_checkpoint_ms:" + route
	}
	return ""
}
func valueOrNil(value string, ok bool) *string {
	if ok {
		return &value
	}
	return nil
}
func kindOrIgnored(kind model.EventKind, ignored bool) model.EventKind {
	if ignored {
		return model.Ignored
	}
	return kind
}
func nameOrIgnored(value string, ignored bool) string {
	if ignored {
		return "ignored"
	}
	return value
}
func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
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
