package discord

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	sdk "github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"

	"github.com/motoki317/noctiluca/pkg/domain"
)

func (a *adapter) file(ctx context.Context, ref string) (*sdk.Attachment, error) {
	parts := strings.Split(ref, ":")
	if len(parts) != 4 || parts[0] != a.name {
		return nil, errors.New("invalid Discord file reference")
	}
	channel, err := parseID(parts[1])
	if err != nil {
		return nil, err
	}
	message, err := parseID(parts[2])
	if err != nil {
		return nil, err
	}
	id, err := parseID(parts[3])
	if err != nil {
		return nil, err
	}
	m, err := a.api.GetMessage(channel, message, rest.WithCtx(ctx))
	if err != nil {
		return nil, discordError("read Discord attachment failed", err)
	}
	for _, file := range m.Attachments {
		if file.ID == id {
			if !cdnURL(file.URL) {
				return nil, errors.New("attachment has no Discord CDN URL")
			}
			return &file, nil
		}
	}
	return nil, errors.New("attachment not found in Discord message")
}

func cdnURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.User == nil && (u.Host == "cdn.discordapp.com" || u.Host == "media.discordapp.net")
}

func (a *adapter) download(ctx context.Context, in toolInput) (any, error) {
	file, err := a.file(ctx, in.FileRef)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(in.Path), 0700); err != nil {
		return nil, err
	}
	out, err := os.OpenFile(in.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(in.Path)
		}
	}()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, file.URL, nil)
	if err != nil {
		return nil, errors.New("invalid Discord attachment URL")
	}
	client := *a.client.HTTPClient()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !cdnURL(req.URL.String()) {
			return errors.New("invalid Discord attachment redirect")
		}
		return nil
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, discordError("download Discord attachment failed", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("download Discord attachment failed")
	}
	if _, err = io.Copy(out, response.Body); err != nil {
		return nil, errors.New("read Discord attachment bytes failed")
	}
	if err = out.Close(); err != nil {
		return nil, err
	}
	ok = true
	return map[string]string{"path": in.Path, "file_ref": in.FileRef}, nil
}

func (a *adapter) upload(ctx context.Context, in toolInput) (any, error) {
	file, err := os.Open(in.Path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() == 0 {
		return nil, errors.New("upload requires a nonempty regular file")
	}
	ref := Conversation(a.name, in.Channel, in.Thread)
	created, err := a.post(ctx, ref, in.Text, []*sdk.File{{Name: filepath.Base(in.Path), Reader: file}})
	if err != nil {
		return nil, err
	}
	id := created.ID.String()
	attachments := a.attachments(*created)
	if err = a.record(ctx, id, ref, domain.OutboundMessage{Text: in.Text, Attachments: attachments}); err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "attachments": attachments}, nil
}
