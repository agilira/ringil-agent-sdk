// voice.go: sending voice messages
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/textproto"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/channel"
)

// maxVoice bounds a voice sent: what the Bot API takes by upload.
const maxVoice = 50 << 20

// upload is a request with a file, sent as a multipart form.
type upload struct {
	fields      map[string]string
	field, name string
	mime        string
	data        []byte
}

// encode is a request's body and type: JSON, or a form for an upload.
func encode(in any) ([]byte, string, error) {
	u, ok := in.(upload)
	if !ok {
		body, err := json.Marshal(in)
		return body, "application/json", err
	}
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for k, v := range u.fields {
		if err := w.WriteField(k, v); err != nil {
			return nil, "", err
		}
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", multipart.FileContentDisposition(u.field, u.name))
	h.Set("Content-Type", u.mime)
	part, err := w.CreatePart(h)
	if err == nil {
		_, err = part.Write(u.data)
	}
	if err == nil {
		err = w.Close()
	}
	return b.Bytes(), w.FormDataContentType(), err
}

// SendVoice sends a voice message: Opus in Ogg shows as one, any other
// audio goes as an audio file.
func (c *Channel) SendVoice(ctx context.Context, chat string, voice channel.Audio) error {
	if len(voice.Data) == 0 || len(voice.Data) > maxVoice {
		return errors.New(ErrAPI, "a voice message must be 1 byte to 50 MB")
	}
	method, field, name, mime := "sendVoice", "voice", "voice.ogg", "audio/ogg"
	if voice.MIME != "audio/ogg" && voice.MIME != "audio/opus" {
		method, field, name, mime = "sendAudio", "audio", "answer.mp3", "audio/mpeg"
		if voice.MIME != "audio/mpeg" {
			name, mime = "answer.bin", "application/octet-stream"
		}
	}
	return c.call(ctx, method, upload{fields: map[string]string{"chat_id": chat}, field: field, name: name, mime: mime, data: voice.Data}, nil)
}
