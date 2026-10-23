package telegram

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

// Rich messages have no ordinary text field. Flatten their documented structure
// before the existing Mattermost escaping/splitting pipeline; never emit raw HTML.
// An unknown block rejects the whole message instead of silently losing content.
func richMessageText(raw json.RawMessage) (string, error) {
	if len(raw) > 2<<20 {
		return "", errors.New("rich_message_limit")
	}
	var message struct {
		Blocks []map[string]any `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		return "", errors.New("invalid_rich_message")
	}
	r := richRenderer{}
	value := r.blocks(message.Blocks, 0)
	if r.failed || strings.TrimSpace(value) == "" {
		return "", errors.New("unsupported_rich_message")
	}
	return value, nil
}

type richRenderer struct {
	nodes  int
	failed bool
}

func (r *richRenderer) enter(depth int) bool {
	r.nodes++
	if depth > 64 || r.nodes > 8192 {
		r.failed = true
		return false
	}
	return true
}

func stringField(m map[string]any, key string) string { s, _ := m[key].(string); return s }

func (r *richRenderer) text(v any, depth int) string {
	if !r.enter(depth) {
		return ""
	}
	switch v := v.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, item := range v {
			b.WriteString(r.text(item, depth+1))
		}
		return b.String()
	case map[string]any:
		switch stringField(v, "type") {
		case "bold", "italic", "underline", "strikethrough", "spoiler", "date_time", "text_mention", "subscript", "superscript", "marked", "code", "email_address", "phone_number", "bank_card_number", "mention", "hashtag", "cashtag", "bot_command", "anchor_link", "reference", "reference_link":
			return r.text(v["text"], depth+1)
		case "url":
			label := r.text(v["text"], depth+1)
			link := stringField(v, "url")
			u, err := url.Parse(link)
			if err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && label != link {
				return label + " (" + link + ")"
			}
			return label
		case "custom_emoji":
			return stringField(v, "alternative_text")
		case "mathematical_expression":
			return stringField(v, "expression")
		case "anchor":
			return ""
		}
	}
	r.failed = true
	return ""
}

func (r *richRenderer) childBlocks(v any, depth int) string {
	items, ok := v.([]any)
	if !ok {
		r.failed = true
		return ""
	}
	blocks := make([]map[string]any, 0, len(items))
	for _, item := range items {
		block, ok := item.(map[string]any)
		if !ok {
			r.failed = true
			continue
		}
		blocks = append(blocks, block)
	}
	return r.blocks(blocks, depth+1)
}

func (r *richRenderer) blocks(blocks []map[string]any, depth int) string {
	if !r.enter(depth) {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if !r.enter(depth) {
			break
		}
		var part string
		switch stringField(block, "type") {
		case "paragraph", "heading", "pre", "footer", "expandable_blockquote", "pullquote":
			part = r.text(block["text"], depth+1)
		case "mathematical_expression":
			part = stringField(block, "expression")
		case "divider":
			part = "────────"
		case "anchor":
			continue
		case "blockquote", "details":
			part = r.childBlocks(block["blocks"], depth+1)
			if stringField(block, "type") == "blockquote" {
				part = "│ " + strings.ReplaceAll(part, "\n", "\n│ ")
			} else if summary := r.text(block["summary"], depth+1); summary != "" {
				part = summary + "\n" + part
			}
		case "list":
			items, ok := block["items"].([]any)
			if !ok {
				r.failed = true
				break
			}
			var lines []string
			for _, v := range items {
				item, ok := v.(map[string]any)
				if !ok {
					r.failed = true
					continue
				}
				label := stringField(item, "label")
				if label == "" {
					label = "•"
				}
				if item["has_checkbox"] == true {
					label = "☐"
					if item["is_checked"] == true {
						label = "☑"
					}
				}
				body := r.childBlocks(item["blocks"], depth+1)
				lines = append(lines, label+" "+strings.ReplaceAll(body, "\n", "\n  "))
			}
			part = strings.Join(lines, "\n")
		case "table":
			rows, ok := block["cells"].([]any)
			if !ok {
				r.failed = true
				break
			}
			var lines []string
			for _, v := range rows {
				cells, ok := v.([]any)
				if !ok {
					r.failed = true
					continue
				}
				var values []string
				for _, v := range cells {
					cell, ok := v.(map[string]any)
					if !ok {
						r.failed = true
						continue
					}
					values = append(values, r.text(cell["text"], depth+1))
				}
				lines = append(lines, strings.Join(values, " | "))
			}
			part = strings.Join(lines, "\n")
			if caption := r.text(block["caption"], depth+1); caption != "" {
				part += "\n" + caption
			}
		default:
			// Rich media and future types require explicit support. Do not claim
			// delivery of just the text while dropping an embedded attachment.
			r.failed = true
		}
		if credit := r.text(block["credit"], depth+1); credit != "" {
			part += "\n" + credit
		}
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "\n\n")
}

func (m *Message) contentText() (string, error) {
	if len(m.RichMessage) > 0 && string(m.RichMessage) != "null" {
		return richMessageText(m.RichMessage)
	}
	if m.Text != "" {
		return m.Text, nil
	}
	return m.Caption, nil
}

// Keep only a bounded, known type name, never unknown field names or values.
func (m *Message) UnmarshalJSON(data []byte) error {
	type wire Message
	var value wire
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*m = Message(value)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	m.ContentType = "unknown"
	for _, name := range []string{"rich_message", "text", "photo", "document", "video", "audio", "voice", "animation", "sticker", "poll", "contact", "location", "venue", "dice", "video_note", "paid_media", "story", "checklist", "game"} {
		if v, ok := fields[name]; ok && string(v) != "null" {
			m.ContentType = name
			return nil
		}
	}
	for _, name := range []string{"new_chat_members", "left_chat_member", "new_chat_title", "new_chat_photo", "delete_chat_photo", "pinned_message", "group_chat_created", "supergroup_chat_created", "channel_chat_created", "migrate_to_chat_id", "migrate_from_chat_id", "message_auto_delete_timer_changed", "video_chat_started", "video_chat_ended", "video_chat_scheduled", "video_chat_participants_invited", "forum_topic_created", "forum_topic_closed", "forum_topic_reopened", "forum_topic_edited"} {
		if _, ok := fields[name]; ok {
			m.ContentType = "service"
			break
		}
	}
	return nil
}
