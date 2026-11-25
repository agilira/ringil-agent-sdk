// files.go: the files of a message, fetched when the Bridge uses them
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package slack

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/channel"
)

type file struct {
	Name     string `json:"name"`
	MIME     string `json:"mimetype"`
	Size     int64  `json:"size"`
	Subtype  string `json:"subtype"`
	Download string `json:"url_private_download"`
}

// maxFiles bounds the files taken from one message.
const maxFiles = 20

// files are the message's files, each fetched with the bot token from
// Slack's own file host only.
func (c *Channel) files(in []file) []channel.File {
	var out []channel.File
	for _, f := range in {
		if len(out) == maxFiles {
			break
		}
		if !fileURL(f.Download, c.fileHost) {
			continue
		}
		u := f.Download
		out = append(out, channel.File{Name: f.Name, MIME: f.MIME, Size: max(f.Size, 0), Voice: f.Subtype == "slack_audio",
			Fetch: func(ctx context.Context, limit int64) ([]byte, error) { return c.fetch(ctx, u, limit) }})
	}
	return out
}

// fileURL: https on host, files.slack.com, where Slack keeps what is
// shared: the bot token goes nowhere else.
func fileURL(raw, host string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.User == nil && strings.EqualFold(u.Host, host)
}

// fetch reads a file, failing past limit bytes without reading more.
func (c *Channel) fetch(ctx context.Context, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errors.Wrap(err, ErrAPI, "slack: bad file address")
	}
	req.Header.Set("Authorization", "Bearer "+c.bot)
	resp, err := c.fetcher.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, ErrNetwork, "slack: cannot fetch the file")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(ErrAPI, "slack: file HTTP "+strconv.Itoa(resp.StatusCode))
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		return nil, errors.New(ErrAPI, "slack: the file needs the files:read scope") // Slack's sign-in page
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, errors.Wrap(err, ErrNetwork, "slack: cannot read the file")
	}
	if int64(len(data)) > limit {
		return nil, errors.New(ErrAPI, "slack: the file is larger than "+strconv.FormatInt(limit, 10)+" bytes")
	}
	return data, nil
}
