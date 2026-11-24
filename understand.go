package main

// Media understanding and compact context.
//
// Voice notes and photos are half of what people send on WhatsApp, and an agent that sees "[audio]" answers
// badly. Processors turn media into text before turn.ready goes out. They are pluggable and named in wago.yaml:
//
//	transcribe  an OpenAI-compatible /audio/transcriptions endpoint (OpenAI, Groq, NVIDIA NIM, a local whisper server)
//	vision      an OpenAI-compatible /chat/completions endpoint with an image (any vision model)
//	http        your own service: the file is POSTed as the body, the answer is {"text": ...} or plain text
//	command     a local program: {file} in the arguments is replaced by a temporary file, stdout is the text
//
// Results are cached per message in agent.sqlite3, so a voice note is transcribed once however often it is read.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Processor struct {
	Type      string            `json:"type"` // transcribe, vision, http, command
	URL       string            `json:"url,omitempty"`
	Model     string            `json:"model,omitempty"`
	APIKey    string            `json:"apiKey,omitempty"` // secret reference (env:, file:, keychain:)
	Prompt    string            `json:"prompt,omitempty"`
	Language  string            `json:"language,omitempty"`
	Command   []string          `json:"command,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	TimeoutMs int               `json:"timeoutMs,omitempty"`

	key string // resolved at start
}

func (p Processor) validate() error {
	switch p.Type {
	case "transcribe", "vision":
		if p.URL == "" || p.Model == "" {
			return fmt.Errorf("%s needs url (the API base, e.g. https://api.openai.com/v1) and model", p.Type)
		}
	case "http":
		if p.URL == "" {
			return fmt.Errorf("http needs url")
		}
	case "command":
		if len(p.Command) == 0 {
			return fmt.Errorf("command needs command: [program, args...] with {file} where the file goes")
		}
	default:
		return fmt.Errorf("type is transcribe, vision, http or command, not %q", p.Type)
	}
	return nil
}

const defaultVisionPrompt = "Describe this image for someone who cannot see it, in the language of any text in it, in at most three sentences. Transcribe any text exactly."

var procClient = &http.Client{}

func (p *Processor) run(ctx context.Context, data []byte, mime string) (string, error) {
	to := 60 * time.Second
	if p.TimeoutMs > 0 {
		to = ms(p.TimeoutMs)
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	switch p.Type {
	case "transcribe":
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		fw, _ := w.CreateFormFile("file", "audio"+extOf(mime))
		fw.Write(data)
		w.WriteField("model", p.Model)
		if p.Language != "" {
			w.WriteField("language", p.Language)
		}
		if p.Prompt != "" {
			w.WriteField("prompt", p.Prompt)
		}
		w.Close()
		var out struct {
			Text string `json:"text"`
		}
		if err := p.call(ctx, strings.TrimRight(p.URL, "/")+"/audio/transcriptions", w.FormDataContentType(), &body, &out); err != nil {
			return "", err
		}
		return strings.TrimSpace(out.Text), nil
	case "vision":
		prompt := p.Prompt
		if prompt == "" {
			prompt = defaultVisionPrompt
		}
		req := map[string]any{"model": p.Model, "max_tokens": 400, "messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": prompt},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)}},
		}}}}
		b, _ := json.Marshal(req)
		var out struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := p.call(ctx, strings.TrimRight(p.URL, "/")+"/chat/completions", "application/json", bytes.NewReader(b), &out); err != nil {
			return "", err
		}
		if len(out.Choices) == 0 {
			return "", fmt.Errorf("the model returned no answer")
		}
		return strings.TrimSpace(out.Choices[0].Message.Content), nil
	case "http":
		var raw json.RawMessage
		if err := p.call(ctx, p.URL, mime, bytes.NewReader(data), &raw); err != nil {
			return "", err
		}
		var out struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &out) == nil && out.Text != "" {
			return strings.TrimSpace(out.Text), nil
		}
		var s string
		json.Unmarshal(raw, &s)
		return strings.TrimSpace(s), nil
	case "command":
		dir, err := os.MkdirTemp("", "wago-proc")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(dir)
		f := filepath.Join(dir, "input"+extOf(mime))
		if err := os.WriteFile(f, data, 0o600); err != nil {
			return "", err
		}
		args := make([]string, len(p.Command))
		for i, a := range p.Command {
			args[i] = strings.ReplaceAll(a, "{file}", f)
		}
		out, err := exec.CommandContext(ctx, args[0], args[1:]...).Output()
		if err != nil {
			return "", fmt.Errorf("%s: %w", args[0], err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	return "", fmt.Errorf("unknown processor type %q", p.Type)
}

func (p *Processor) call(ctx context.Context, url, ctype string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, "POST", url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", ctype)
	if p.key != "" {
		req.Header.Set("Authorization", "Bearer "+p.key)
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	resp, err := procClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s answered %d: %.200s", url, resp.StatusCode, b)
	}
	if err := json.Unmarshal(b, out); err != nil {
		if raw, ok := out.(*json.RawMessage); ok { // plain text answer
			*raw, _ = json.Marshal(string(b))
			return nil
		}
		return fmt.Errorf("%s: %w", url, err)
	}
	return nil
}

func extOf(mime string) string {
	mime, _, _ = strings.Cut(mime, ";")
	switch strings.TrimSpace(mime) {
	case "audio/ogg":
		return ".ogg"
	case "audio/mpeg":
		return ".mp3"
	case "audio/mp4", "audio/aac":
		return ".m4a"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "application/pdf":
		return ".pdf"
	}
	return ""
}

// processorFor picks the processor configured for a mime type.
func processorFor(mime string, cfg UnderstandConfig) string {
	switch {
	case strings.HasPrefix(mime, "audio/"):
		return cfg.Audio
	case strings.HasPrefix(mime, "image/"):
		return cfg.Image
	case strings.HasPrefix(mime, "video/"):
		return cfg.Video
	case mime != "":
		return cfg.Document
	}
	return ""
}

// understand returns the text of a media message (cached), or "" when there is no media or no processor for it.
func (e *Engine) understand(ctx context.Context, chat, id string, cfg AgentConfig) string {
	if t, ok := e.db.understood(chat, tail(id)); ok {
		return t
	}
	m, err := e.hist.get(ctx, chat, id)
	if err != nil || m.MediaType == "" {
		return ""
	}
	name := processorFor(m.MediaType, cfg.Understand)
	p := e.s.mgr.processors[name]
	if p == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, ms(cfg.Understand.TimeoutMs))
	defer cancel()
	data, mime, err := e.hist.media(ctx, chat, id)
	if err != nil {
		return ""
	}
	if mime == "" {
		mime = m.MediaType
	}
	text, err := p.run(ctx, data, mime)
	if err != nil || text == "" {
		if err != nil {
			e.emit("understanding.failed", map[string]any{"chatId": chat, "messageId": id, "processor": name, "error": err.Error()})
		}
		return ""
	}
	e.db.saveUnderstanding(chat, tail(id), text, name, e.now())
	return text
}

// ---------------------------------------------------------------- context

// CtxLine is one message of the compact history: who, when, what, with media already turned into text.
type CtxLine struct {
	ID   string `json:"id"`
	Role string `json:"role"` // user (the other side), agent (sent through this API), human (written on a device by a person)
	Name string `json:"name,omitempty"`
	At   int64  `json:"at"`
	Text string `json:"text"`
}

// context returns the chat's recent messages up to `upto`, oldest first, within the message and character budget.
// Cheap to send with every turn, and the agent needs nothing else to answer in context.
func (e *Engine) context(ctx context.Context, chat string, upto time.Time, c ContextConfig, cfg AgentConfig) []CtxLine {
	if c.Messages <= 0 {
		return []CtxLine{}
	}
	rows, err := e.hist.recent(ctx, chat, upto, c.Messages)
	if err != nil {
		return []CtxLine{}
	}
	out := make([]CtxLine, 0, len(rows))
	used := 0
	for _, r := range rows {
		l := CtxLine{ID: r.ID, At: r.At.Unix(), Text: r.Body}
		switch {
		case !r.FromMe:
			l.Role, l.Name = "user", r.SenderName
		case e.s.adb() != nil && e.s.adb().wasSent(r.ID), e.db.wasSent(r.ID):
			l.Role = "agent"
		default:
			l.Role = "human"
		}
		if r.MediaType != "" {
			kind, _, _ := strings.Cut(r.MediaType, "/")
			if kind == "application" {
				kind = "document"
			}
			u, _ := e.db.understood(chat, r.ID)
			l.Text = strings.TrimSpace("[" + kind + "] " + strings.TrimSpace(u+"\n"+r.Body))
		}
		if l.Text == "" {
			continue
		}
		if c.Chars > 0 && used+len(l.Text) > c.Chars {
			break
		}
		used += len(l.Text)
		out = append(out, l)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
