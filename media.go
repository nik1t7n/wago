package main

// Media on demand. The mirror keeps each message's media keys (the media table); a file is fetched from WhatsApp's
// servers the first time someone opens it and is then kept on disk under its content hash, so it opens instantly
// afterwards and survives the servers' expiry. WhatsApp keeps files on its servers for a few weeks only; an older
// file is asked from the phone again (a media retry receipt), which is what WhatsApp Web does for old history.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waMmsRetry"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// mediaRef is what the mirror knows about one message's file.
type mediaRef struct {
	chat, id, sender string
	fromMe           bool
	blob             []byte // serialized waE2E.Message holding the keys
}

func downloadable(m *waE2E.Message) whatsmeow.DownloadableMessage {
	switch {
	case m.GetImageMessage() != nil:
		return m.GetImageMessage()
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage()
	case m.GetAudioMessage() != nil:
		return m.GetAudioMessage()
	case m.GetDocumentMessage() != nil:
		return m.GetDocumentMessage()
	case m.GetStickerMessage() != nil:
		return m.GetStickerMessage()
	}
	return nil
}

func setDirectPath(d whatsmeow.DownloadableMessage, p string) {
	switch v := d.(type) {
	case *waE2E.ImageMessage:
		v.DirectPath = &p
	case *waE2E.VideoMessage:
		v.DirectPath = &p
	case *waE2E.AudioMessage:
		v.DirectPath = &p
	case *waE2E.DocumentMessage:
		v.DirectPath = &p
	case *waE2E.StickerMessage:
		v.DirectPath = &p
	}
}

// flights makes concurrent requests for one file share a single download.
type flights struct {
	mu sync.Mutex
	m  map[string]*flight
}

type flight struct {
	done chan struct{}
	data []byte
	err  error
}

func (f *flights) do(key string, fn func() ([]byte, error)) ([]byte, error) {
	f.mu.Lock()
	if f.m == nil {
		f.m = map[string]*flight{}
	}
	if c, ok := f.m[key]; ok {
		f.mu.Unlock()
		<-c.done
		return c.data, c.err
	}
	c := &flight{done: make(chan struct{})}
	f.m[key] = c
	f.mu.Unlock()
	c.data, c.err = fn()
	close(c.done)
	f.mu.Lock()
	delete(f.m, key)
	f.mu.Unlock()
	return c.data, c.err
}

func (s *Session) mediaFile(ctx context.Context, ref mediaRef) ([]byte, error) {
	var msg waE2E.Message
	if err := proto.Unmarshal(ref.blob, &msg); err != nil {
		return nil, err
	}
	d := downloadable(&msg)
	if d == nil {
		return nil, httpError{404, "this message has no file"}
	}
	key := hex.EncodeToString(d.GetFileSHA256())
	if key == "" {
		h := sha256.Sum256([]byte(ref.chat + "/" + ref.id))
		key = "m" + hex.EncodeToString(h[:])
	}
	path := filepath.Join(s.dir, "files", key[:2], key)
	if b, err := os.ReadFile(path); err == nil {
		return b, nil
	}
	return s.downloads.do(key, func() ([]byte, error) {
		cli := s.Client()
		if cli == nil || !cli.IsConnected() {
			return nil, httpError{503, "not connected to WhatsApp, and this file has not been fetched before"}
		}
		data, err := cli.Download(ctx, d)
		if expired(err) {
			data, err = s.retryMedia(ctx, cli, ref, &msg, d)
		}
		if err != nil {
			var he httpError
			if errors.As(err, &he) {
				return nil, he
			}
			return nil, httpError{502, "download failed: " + err.Error()}
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
			tmp := path + ".part"
			if os.WriteFile(tmp, data, 0o600) == nil {
				os.Rename(tmp, path)
			}
		}
		return data, nil
	})
}

// retryMedia asks the phone to upload an expired file again and downloads it from the new path.
func (s *Session) retryMedia(ctx context.Context, cli *whatsmeow.Client, ref mediaRef, msg *waE2E.Message, d whatsmeow.DownloadableMessage) ([]byte, error) {
	chat, err := parseChatID(ref.chat)
	if err != nil {
		return nil, err
	}
	info := types.MessageInfo{ID: ref.id, MessageSource: types.MessageSource{Chat: chat, IsFromMe: ref.fromMe, IsGroup: chat.Server == types.GroupServer}}
	if info.IsGroup && !ref.fromMe {
		if info.Sender, err = parseChatID(ref.sender); err != nil {
			return nil, httpError{502, "the file expired on WhatsApp's servers and the sender is unknown"}
		}
	}
	ch := make(chan *events.MediaRetry, 1)
	s.mu.Lock()
	if s.retries == nil {
		s.retries = map[string]chan *events.MediaRetry{}
	}
	s.retries[ref.id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.retries, ref.id)
		s.mu.Unlock()
	}()
	if err := cli.SendMediaRetryReceipt(ctx, &info, d.GetMediaKey()); err != nil {
		return nil, fmt.Errorf("asking the phone for the file: %w", err)
	}
	var evt *events.MediaRetry
	select {
	case evt = <-ch:
	case <-time.After(40 * time.Second):
		return nil, httpError{504, "the file expired on WhatsApp's servers and the phone did not send it again (is it online?)"}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	n, err := whatsmeow.DecryptMediaRetryNotification(evt, d.GetMediaKey())
	if errors.Is(err, whatsmeow.ErrMediaNotAvailableOnPhone) {
		return nil, httpError{410, "the file is no longer on the phone either"}
	}
	if err != nil {
		return nil, err
	}
	if n.GetResult() != waMmsRetry.MediaRetryNotification_SUCCESS || n.GetDirectPath() == "" {
		return nil, httpError{410, "the phone could not send the file again: " + n.GetResult().String()}
	}
	setDirectPath(d, n.GetDirectPath())
	if b, err := proto.Marshal(msg); err == nil {
		if m, err := s.Mirror(); err == nil {
			m.Push(Op{MediaChat: ref.chat, MediaID: ref.id, MediaMsg: b}) // the new path, for the next device or cache loss
		}
	}
	return cli.Download(ctx, d)
}

// expired: WhatsApp's servers answer 403, 404 or 410 for a file they no longer keep; the phone can upload it again.
func expired(err error) bool {
	return errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403) || errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410)
}

func (s *Session) onMediaRetry(v *events.MediaRetry) {
	s.mu.Lock()
	ch := s.retries[v.MessageID]
	s.mu.Unlock()
	if ch != nil {
		select {
		case ch <- v:
		default:
		}
	}
}
