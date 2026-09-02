package mattermost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/config"
	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/model"
)

func TestEventFromPostNormalizesAuthorAndFiltersBot(t *testing.T) {
	bot := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/users/u" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(User{ID: "u", Username: "anton", FirstName: "Антон", LastName: "Тестов", IsBot: bot})
	}))
	defer server.Close()
	adapter := New(server.Client(), server.URL, "secret", []config.Route{{ID: "default", TGChatID: -1, MMChannelID: "c"}}, nil, nil)
	post := Post{ID: "p", ChannelID: "c", UserID: "u", Message: "Спасибо", CreateAt: 100}
	event, err := adapter.EventFromPost(context.Background(), post, model.Message, "self")
	if err != nil || event.Kind != model.Message || event.AuthorName != "Антон Тестов" || event.RouteID != "default" {
		t.Fatalf("event %#v %v", event, err)
	}
	adapter.mu.Lock()
	delete(adapter.users, "u")
	adapter.mu.Unlock()
	bot = true
	event, err = adapter.EventFromPost(context.Background(), post, model.Message, "self")
	if err != nil || event.Kind != model.Ignored {
		t.Fatalf("bot event %#v %v", event, err)
	}
}

func TestSendUsesCompactAuthorAndOverrideProps(t *testing.T) {
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/config/client":
			fmt.Fprint(w, `{"EnablePostUsernameOverride":"true","EnablePostIconOverride":"false"}`)
		case "/api/v4/posts":
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Fatal(err)
			}
			fmt.Fprint(w, `{"id":"mm1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter := New(server.Client(), server.URL, "secret", []config.Route{{ID: "default", TGChatID: -1, MMChannelID: "c"}}, nil, nil)
	event := model.Event{Platform: model.Telegram, Kind: model.Message, AuthorID: "1", AuthorName: "Антон", AuthorUsername: "anton", RouteID: "default", Text: "Спасибо"}
	var sent model.SentMessage
	err := adapter.Send(context.Background(), event, model.DeliveryContext{}, nil, nil, 0, func(value model.SentMessage) error { sent = value; return nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if posted["message"] != "*Антон: *Спасибо" || sent.MessageID != "mm1" {
		t.Fatalf("posted %#v sent %#v", posted, sent)
	}
	props := posted["props"].(map[string]any)
	if props["override_username"] == nil {
		t.Fatalf("missing override props %#v", props)
	}
}

func TestHTTPErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		status int
		retry  bool
	}{{429, true}, {503, true}, {403, false}} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status == 429 {
					w.Header().Set("Retry-After", "3")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, `{}`)
			}))
			defer server.Close()
			adapter := New(server.Client(), server.URL, "secret", []config.Route{{ID: "default", TGChatID: -1, MMChannelID: "c"}}, nil, nil)
			_, err := adapter.GetMe(context.Background())
			var deliveryErr *model.DeliveryError
			if !errors.As(err, &deliveryErr) || deliveryErr.Retryable != tc.retry {
				t.Fatalf("error %#v", err)
			}
			if tc.status == 429 && deliveryErr.RetryAfter != 3*time.Second {
				t.Fatalf("retry after %v", deliveryErr.RetryAfter)
			}
		})
	}
}

func TestDecodePossiblyString(t *testing.T) {
	var post Post
	if !decodePossiblyString(json.RawMessage(`"{\"id\":\"p\"}"`), &post) || post.ID != "p" {
		t.Fatalf("post %#v", post)
	}
}

func TestRunWebSocketConsumesPostedEvent(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/channels/c/posts":
			fmt.Fprint(w, `{"posts":{}}`)
		case "/api/v4/users/u":
			json.NewEncoder(w).Encode(User{ID: "u", FirstName: "Антон"})
		case "/api/v4/websocket":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			var auth map[string]any
			if err := conn.ReadJSON(&auth); err != nil {
				return
			}
			post := `{"id":"p1","channel_id":"c","user_id":"u","message":"hello","create_at":101}`
			_ = conn.WriteJSON(map[string]any{"event": "posted", "data": map[string]string{"post": post}, "broadcast": map[string]string{"channel_id": "c"}})
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter := New(server.Client(), server.URL, "secret", []config.Route{{ID: "default", TGChatID: -1, MMChannelID: "c"}}, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	received := make(chan model.Event, 1)
	done := make(chan error, 1)
	go func() {
		done <- adapter.RunWebSocket(ctx, map[string]int64{"default": 100}, "self", func(_ context.Context, event model.Event) (bool, error) {
			received <- event
			cancel()
			return true, nil
		})
	}()
	select {
	case event := <-received:
		if event.MessageID != "p1" || event.AuthorName != "Антон" || event.Text != "hello" {
			t.Fatalf("event %#v", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("websocket event timeout")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("websocket did not stop")
	}
}
