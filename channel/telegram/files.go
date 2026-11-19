// files.go: photos, voice, audio, documents and videos sent to the bot
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package telegram

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/channel"
)

// files lists what a message carries, to be fetched when used.
func (c *Channel) files(m *message) []channel.File {
	var out []channel.File
	if p, ok := bestPhoto(m.Photo); ok {
		// Telegram re-encodes photos as JPEG.
		out = append(out, c.file(media{FileID: p.FileID, Size: p.Size, MIME: "image/jpeg", Name: "photo.jpg"}, false))
	}
	if m.Voice != nil {
		out = append(out, c.file(withMIME(*m.Voice, "audio/ogg"), true))
	}
	for _, x := range []*media{m.Audio, m.Document, m.Video, m.Note} {
		if x != nil {
			out = append(out, c.file(withMIME(*x, "application/octet-stream"), false))
		}
	}
	return out
}

// bestPhoto is the largest size of a photo that is still an image the
// model APIs take. Telegram lists them smallest first, and may leave out
// their sizes: then the last.
func bestPhoto(sizes []photoSize) (photoSize, bool) {
	best := -1
	for i, p := range sizes {
		if p.Size == 0 || p.Size <= agent.MaxImageBytes {
			best = i
		}
	}
	switch {
	case len(sizes) == 0:
		return photoSize{}, false
	case best < 0:
		return sizes[0], true // none fits: the smallest, which the Bridge refuses with its reason
	}
	return sizes[best], true
}

func withMIME(m media, fallback string) media {
	if m.MIME == "" {
		m.MIME = fallback
	}
	return m
}

func (c *Channel) file(m media, voice bool) channel.File {
	id := m.FileID
	return channel.File{Name: m.Name, MIME: m.MIME, Size: m.Size, Voice: voice,
		Fetch: func(ctx context.Context, max int64) ([]byte, error) { return c.download(ctx, id, max) }}
}

// filePath is a path the Bot API gives for a file: never one that leaves
// its directory.
var filePath = regexp.MustCompile(`^[A-Za-z0-9_-]+(/[A-Za-z0-9_.-]+)*$`)

// downloadTimeout bounds one file's download; a variable for tests.
var downloadTimeout = 2 * time.Minute

// download fetches a file, failing past max bytes without reading more.
func (c *Channel) download(ctx context.Context, fileID string, max int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	var f struct {
		Path string `json:"file_path"`
		Size int64  `json:"file_size"`
	}
	if err := c.call(ctx, "getFile", map[string]string{"file_id": fileID}, &f); err != nil {
		return nil, err
	}
	if !filePath.MatchString(f.Path) || strings.Contains(f.Path, "..") {
		return nil, errors.New(ErrAPI, "telegram getFile: unusable file path")
	}
	if f.Size > max {
		return nil, tooLarge(max)
	}
	return c.get(ctx, f.Path, max)
}

func tooLarge(max int64) error {
	return errors.New(ErrAPI, "the file is larger than "+strconv.FormatInt(max, 10)+" bytes")
}

// get reads a file of the Bot API's file storage.
func (c *Channel) get(ctx context.Context, path string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/file/bot"+c.token+"/"+path, nil)
	if err != nil {
		return nil, errors.New(ErrNetwork, "telegram file: bad request")
	}
	resp, err := c.fetcher.Do(req)
	if err != nil {
		return nil, errors.New(ErrNetwork, "telegram file: "+c.scrub(netError(err)))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(ErrAPI, "telegram file: HTTP "+strconv.Itoa(resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	switch {
	case err != nil:
		return nil, errors.New(ErrNetwork, "telegram file: "+c.scrub(err.Error()))
	case int64(len(data)) > max:
		return nil, tooLarge(max)
	}
	return data, nil
}
