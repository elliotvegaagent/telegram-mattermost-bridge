package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/config"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/model"
)

func TestHTTPErrorClassification(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		retryable  bool
		retryAfter time.Duration
	}{{"rate", 429, `{"ok":false,"parameters":{"retry_after":7}}`, true, 7 * time.Second}, {"server", 503, `{"ok":false}`, true, 0}, {"forbidden", 403, `{"ok":false,"description":"Forbidden"}`, false, 0}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer server.Close()
			adapter := New(server.Client(), "secret", server.URL, []config.Route{{ID: "default", TGChatID: -1, MMChannelID: "c"}}, nil, nil)
			_, err := adapter.GetMe(context.Background())
			var deliveryErr *model.DeliveryError
			if !errors.As(err, &deliveryErr) {
				t.Fatalf("expected DeliveryError, got %v", err)
			}
			if deliveryErr.Retryable != tc.retryable || deliveryErr.RetryAfter != tc.retryAfter {
				t.Fatalf("error %#v", deliveryErr)
			}
		})
	}
}

func TestHTTPGetMeSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botsecret/getMe" {
			t.Fatalf("path %q", r.URL.Path)
		}
		fmt.Fprint(w, `{"ok":true,"result":{"id":42,"username":"bridge"}}`)
	}))
	defer server.Close()
	adapter := New(server.Client(), "secret", server.URL, []config.Route{{ID: "default", TGChatID: -1, MMChannelID: "c"}}, nil, nil)
	me, err := adapter.GetMe(context.Background())
	if err != nil || me.ID != 42 || me.Username != "bridge" {
		t.Fatalf("me %#v %v", me, err)
	}
}

func TestRunPollingPersistsBeforeOffsetAdvance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true,"result":[{"update_id":5,"message":{"message_id":8,"from":{"id":2,"first_name":"Антон"},"chat":{"id":-1},"date":10,"text":"hello"}}]}`)
	}))
	defer server.Close()
	adapter := New(server.Client(), "secret", server.URL, []config.Route{{ID: "default", TGChatID: -1, MMChannelID: "c"}}, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	received := make(chan model.Event, 1)
	done := make(chan error, 1)
	go func() {
		done <- adapter.RunPolling(ctx, 0, 99, func(_ context.Context, event model.Event) (bool, error) {
			received <- event
			cancel()
			return true, nil
		})
	}()
	select {
	case event := <-received:
		if event.MessageID != "8" || event.CheckpointValue == nil || *event.CheckpointValue != "6" {
			t.Fatalf("event %#v", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("polling timeout")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("polling did not stop")
	}
}

func TestSendMarksMattermostAuthorAsItalicEntity(t *testing.T) {
	var received url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botsecret/sendMessage" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		received = r.PostForm
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":77}}`)
	}))
	defer server.Close()
	adapter := New(server.Client(), "secret", server.URL, []config.Route{{ID: "default", TGChatID: -1, MMChannelID: "c"}}, nil, nil)
	event := model.Event{Platform: model.Mattermost, Kind: model.Message, AuthorName: "Евгений Воропаев", RouteID: "default", Text: "Спасибо"}
	err := adapter.Send(context.Background(), event, model.DeliveryContext{}, nil, nil, 0, func(model.SentMessage) error { return nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := received.Get("text"); got != "Евгений Воропаев: Спасибо" {
		t.Fatalf("text %q", got)
	}
	var entities []messageEntity
	if err := json.Unmarshal([]byte(received.Get("entities")), &entities); err != nil {
		t.Fatal(err)
	}
	if len(entities) != 1 || entities[0].Type != "italic" || entities[0].Offset != 0 || entities[0].Length != 18 {
		t.Fatalf("entities %#v", entities)
	}
}

func TestDeleteUsesIdempotentBatchForEveryLinkedMessage(t *testing.T) {
	var deleted []int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botsecret/deleteMessages" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(r.PostForm.Get("message_ids")), &deleted); err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	}))
	defer server.Close()
	adapter := New(server.Client(), "secret", server.URL, []config.Route{{ID: "default", TGChatID: -1, MMChannelID: "c"}}, nil, nil)
	event := model.Event{Platform: model.Mattermost, Kind: model.Delete, RouteID: "default"}
	if err := adapter.Delete(context.Background(), event, []string{"10", "11"}); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 2 || deleted[0] != 10 || deleted[1] != 11 {
		t.Fatalf("deleted %#v", deleted)
	}
}
