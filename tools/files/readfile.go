// readfile.go: a whole file, for a host's tools
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package files

import (
	"context"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	agent "github.com/agilira/ringil-agent-sdk"
)

// ReadFile implements Workspace.
func (w *workspace) ReadFile(ctx context.Context, p string, max int64) ([]byte, string, error) {
	if max <= 0 {
		return nil, "", agent.ToolError(ErrInput, "The size bound must be positive.")
	}
	abs, err := w.resolve(ctx, p)
	if err != nil {
		return nil, "", err
	}
	o, err := w.open(abs)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = o.close() }()
	switch {
	case o.info.IsDir():
		return nil, "", agent.ToolError(ErrInput, "That is a directory, not a file.")
	case o.info.Size() > max:
		return nil, "", agent.ToolError(ErrInput, "The file is larger than "+formatLimit(max)+".")
	}
	data, err := io.ReadAll(io.LimitReader(o.f, max+1))
	if err != nil {
		return nil, "", statError(err)
	}
	if int64(len(data)) > max {
		return nil, "", agent.ToolError(ErrInput, "The file grew past "+formatLimit(max)+" while it was read.")
	}
	return data, mediaTypeFor(o.real, p, data), nil
}

func formatLimit(n int64) string {
	if n >= 1<<20 && n%(1<<20) == 0 {
		return strconv.FormatInt(n>>20, 10) + " MiB"
	}
	return strconv.FormatInt(n, 10) + " bytes"
}

func mediaTypeFor(real, req string, data []byte) string {
	if filepath.Ext(real) != "" {
		return mediaType(real, data)
	}
	return mediaType(req, data)
}

// ownTypes are what the system's table may not know.
var ownTypes = map[string]string{
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".odt":  "application/vnd.oasis.opendocument.text",
	".ods":  "application/vnd.oasis.opendocument.spreadsheet",
	".odp":  "application/vnd.oasis.opendocument.presentation",
	".csv":  "text/csv",
	".md":   "text/markdown",
	".json": "application/json",
	".zip":  "application/zip",
}

// mediaType is a file's type by its extension, else by its first bytes.
func mediaType(name string, data []byte) string {
	ext := strings.ToLower(filepath.Ext(name))
	if t, ok := ownTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		if media, _, err := mime.ParseMediaType(t); err == nil {
			return media
		}
	}
	media, _, _ := mime.ParseMediaType(http.DetectContentType(data))
	return media
}
