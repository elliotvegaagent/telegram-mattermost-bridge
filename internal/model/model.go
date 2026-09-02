package model

import (
	"encoding/json"
	"fmt"
	"time"
)

type Platform string

const (
	Telegram   Platform = "telegram"
	Mattermost Platform = "mattermost"
)

func (p Platform) Opposite() Platform {
	if p == Telegram {
		return Mattermost
	}
	return Telegram
}

type EventKind string

const (
	Message         EventKind = "message"
	Edit            EventKind = "edit"
	Delete          EventKind = "delete"
	ReactionAdded   EventKind = "reaction_added"
	ReactionRemoved EventKind = "reaction_removed"
	Ignored         EventKind = "ignored"
)

type Attachment struct {
	FileID          string `json:"file_id"`
	FileName        string `json:"file_name"`
	MimeType        string `json:"mime_type"`
	Size            *int64 `json:"size,omitempty"`
	SourceMessageID string `json:"source_message_id,omitempty"`
}

type MaterializedAttachment struct {
	FileName        string
	MimeType        string
	Data            []byte
	SourceMessageID string
}

type Event struct {
	EventID         string       `json:"event_id"`
	Platform        Platform     `json:"platform"`
	Kind            EventKind    `json:"kind"`
	MessageID       string       `json:"message_id"`
	MessageIDs      []string     `json:"message_ids"`
	AuthorID        string       `json:"author_id"`
	AuthorName      string       `json:"author_name"`
	RouteID         string       `json:"route_id"`
	AuthorUsername  string       `json:"author_username,omitempty"`
	Text            string       `json:"text"`
	ReplyToID       string       `json:"reply_to_id,omitempty"`
	ReplyQuote      string       `json:"reply_quote,omitempty"`
	RootID          string       `json:"root_id,omitempty"`
	CreatedAtMS     int64        `json:"created_at_ms"`
	Attachments     []Attachment `json:"attachments"`
	Reaction        string       `json:"reaction,omitempty"`
	CheckpointKey   string       `json:"checkpoint_key,omitempty"`
	CheckpointValue *string      `json:"checkpoint_value,omitempty"`
}

func (e Event) JSON() ([]byte, error) {
	if len(e.MessageIDs) == 0 {
		e.MessageIDs = []string{e.MessageID}
	}
	if e.RouteID == "" {
		e.RouteID = "default"
	}
	if e.Attachments == nil {
		e.Attachments = []Attachment{}
	}
	return json.Marshal(e)
}

type MessageLink struct {
	MMPostID          string
	TGChatID          int64
	TGMessageID       string
	MMRootID          string
	TGAnchorMessageID string
	PartIndex         int
}

type DeliveryContext struct {
	ReplyToID         string
	RootID            string
	AnchorID          string
	UnknownReplyQuote string
	EditTargetID      string
	DeleteTargetIDs   []string
}

type SentMessage struct {
	MessageID string
	RootID    string
	PartIndex int
}

type OutboxJob struct {
	ID       int64
	EventID  string
	Target   Platform
	Attempts int
	Progress map[string]any
}

type DeliveryError struct {
	Message    string
	Retryable  bool
	RetryAfter time.Duration
}

func (e *DeliveryError) Error() string { return e.Message }

func Retryable(message string, retryAfter time.Duration) error {
	return &DeliveryError{Message: message, Retryable: true, RetryAfter: retryAfter}
}

func Permanent(message string) error {
	return &DeliveryError{Message: message, Retryable: false}
}

type AttachmentRejected struct{ Reason string }

func (e *AttachmentRejected) Error() string { return e.Reason }

func RequiredString(value any, name string) (string, error) {
	s, ok := value.(string)
	if !ok || s == "" {
		return "", fmt.Errorf("%s is missing", name)
	}
	return s, nil
}
