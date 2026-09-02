package textutil

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/model"
)

var (
	mmMarkdown  = regexp.MustCompile("([\\\\`*_{}\\[\\]()<>#+\\-.!|~])")
	mmMention   = regexp.MustCompile(`@([\pL\pN_.-])`)
	badFilename = regexp.MustCompile(`[\r\n\t]+`)
)

var markers = []string{"🔵", "🟢", "🟣", "🟠", "🟡", "🟤", "⚫"}

func AuthorMarker(event model.Event) string {
	sum := sha256.Sum256([]byte(string(event.Platform) + ":" + event.AuthorID))
	return markers[int(sum[0])%len(markers)]
}

func AuthorLabel(event model.Event) string {
	suffix := "Telegram"
	if event.Platform == model.Mattermost {
		suffix = "Mattermost"
	}
	username := ""
	if event.AuthorUsername != "" {
		username = " (@" + event.AuthorUsername + ")"
	}
	return fmt.Sprintf("%s %s · %s%s", AuthorMarker(event), event.AuthorName, suffix, username)
}

func Render(event model.Event, target model.Platform, unknownQuote string, includeAuthor bool) string {
	marker := ""
	if event.Kind == model.Edit {
		marker = "✏️ Исправление"
	}
	if event.Kind == model.Delete {
		marker = "🗑️ Сообщение удалено"
	}
	body := strings.TrimSpace(event.Text)
	if unknownQuote != "" {
		quote := strings.TrimSpace(strings.ReplaceAll(unknownQuote, "\n", " "))
		quote = truncateRunes(quote, 240)
		body = strings.TrimSpace(fmt.Sprintf("↩ Ответ на несвязанное сообщение: «%s»\n\n%s", quote, body))
	}
	if marker != "" {
		body = strings.TrimRight(marker+": "+body, ": ")
	}
	var result string
	if includeAuthor && event.Platform == model.Mattermost && target == model.Telegram {
		result = strings.TrimSpace(event.AuthorName + "\n" + body)
	} else if includeAuthor && event.Platform == model.Telegram && target == model.Mattermost {
		result = strings.TrimRight(event.AuthorName+": "+body, ": ")
	} else if includeAuthor {
		result = strings.TrimSpace(AuthorLabel(event) + "\n" + body)
	} else {
		result = body
	}
	if target == model.Mattermost {
		result = mmMarkdown.ReplaceAllString(result, `\$1`)
		result = mmMention.ReplaceAllString(result, "@\u200b$1")
	}
	return result
}

func Split(value string, limit int) []string {
	if utf8.RuneCountInString(value) <= limit {
		return []string{value}
	}
	runes := []rune(value)
	chunkSize := limit - 12
	if chunkSize < 1 {
		chunkSize = 1
	}
	chunks := []string{}
	for len(runes) > 0 {
		if len(runes) <= chunkSize {
			chunks = append(chunks, string(runes))
			break
		}
		cut := chunkSize
		for i := chunkSize; i >= chunkSize/2; i-- {
			if runes[i-1] == '\n' {
				cut = i - 1
				break
			}
		}
		if cut == chunkSize {
			for i := chunkSize; i >= chunkSize/2; i-- {
				if runes[i-1] == ' ' {
					cut = i - 1
					break
				}
			}
		}
		if cut < 1 {
			cut = chunkSize
		}
		chunks = append(chunks, strings.TrimRight(string(runes[:cut]), " \n\t"))
		runes = []rune(strings.TrimLeft(string(runes[cut:]), " \n\t"))
	}
	result := make([]string, len(chunks))
	for i, chunk := range chunks {
		result[i] = fmt.Sprintf("(%d/%d) %s", i+1, len(chunks), chunk)
	}
	return result
}

func SafeFilename(name string) string {
	name = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(name, "/", "_"), "\\", "_"), "\x00", "")
	name = strings.TrimSpace(badFilename.ReplaceAllString(name, " "))
	name = truncateRunes(name, 240)
	if name == "" {
		return "attachment"
	}
	return name
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
