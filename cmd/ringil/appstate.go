// appstate.go: exporting and importing the state from the app
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	"github.com/agilira/ringil-agent-sdk/backup"
	"github.com/agilira/ringil-agent-sdk/serve"
)

// pauseWait is how long an export or an import waits for the runs going.
const pauseWait = 30 * time.Second

// importing is the import the app asked for: applied once serve has
// stopped and let the state go, then serve starts again.
type importing struct {
	mu        sync.Mutex
	receiving bool   // an upload is on its way
	archive   string // the archive taken, to import when serve has ended
	exports   int    // exports streaming now: an import waits for them
	stop      func() // stops serve
	ctx       *orpheus.Context
}

// mountState mounts, behind the guard:
//
//	GET  /v1/state/export   the state's archive, as a download
//	POST /v1/state/import   an archive as the body: 202, serve stops, imports it and starts again
//	GET  /v1/state/import   how the last import from the app went
func (c *cli) mountState(ctx *orpheus.Context, srv *serve.Server) {
	c.imports.ctx = ctx
	srv.Mount("GET /v1/state/export", c.exportState(ctx, srv))
	srv.Mount("POST /v1/state/import", c.importFromApp(srv))
	srv.Mount("GET /v1/state/import", http.HandlerFunc(c.lastImport))
}

// startExport counts an export in, unless an import is on its way.
func (im *importing) startExport() bool {
	im.mu.Lock()
	defer im.mu.Unlock()
	if im.receiving || im.archive != "" {
		return false
	}
	im.exports++
	return true
}

func (im *importing) endExport() {
	im.mu.Lock()
	im.exports--
	im.mu.Unlock()
}

func (c *cli) exportState(ctx *orpheus.Context, srv *serve.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !c.imports.startExport() {
			fail(w, http.StatusConflict, "STATE_BUSY", "an import is on its way")
			return
		}
		defer c.imports.endExport()
		resume, err := srv.Pause(r.Context(), pauseWait)
		if err != nil {
			fail(w, http.StatusConflict, "STATE_BUSY", describe(err))
			return
		}
		defer resume()
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Hour)) // an archive of gigabytes
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="ringil-`+time.Now().Format("20060102-1504")+`.tar.gz"`)
		w.WriteHeader(http.StatusOK)
		if _, err := c.exportTo(ctx, w); err != nil {
			panic(http.ErrAbortHandler) // the download fails, rather than ending as a short archive
		}
	}
}

// importFromApp takes the archive, checks it, and has serve stop with the
// runs paused: the import runs when nothing holds the state.
func (c *cli) importFromApp(srv *serve.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		im := c.imports
		im.mu.Lock()
		if im.receiving || im.archive != "" {
			im.mu.Unlock()
			fail(w, http.StatusConflict, "STATE_BUSY", "an import is already on its way")
			return
		}
		im.receiving = true // no export starts from here on
		im.mu.Unlock()
		archive, err := c.takeArchive(w, r, srv)
		im.mu.Lock()
		im.receiving = false
		if err == nil {
			im.archive = archive
		}
		im.mu.Unlock()
		switch {
		case errors.HasCode(err, backup.ErrDamaged), errors.HasCode(err, backup.ErrInput):
			fail(w, http.StatusBadRequest, "STATE_ARCHIVE", describe(err))
			return
		case err != nil:
			fail(w, http.StatusConflict, "STATE_BUSY", describe(err))
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"restarting": true,
			"message": "serve stops, imports the archive after a copy of the state as it is, and starts again"})
		go func() {
			time.Sleep(200 * time.Millisecond) // the answer leaves first
			if c.imports.stop != nil {
				c.imports.stop()
			}
		}()
	}
}

// takeArchive receives and checks the archive, waits for the exports
// streaming, and pauses the runs until serve ends.
func (c *cli) takeArchive(w http.ResponseWriter, r *http.Request, srv *serve.Server) (string, error) {
	archive, err := c.receiveArchive(w, r)
	if err != nil {
		return "", err
	}
	err = c.imports.waitExports(r.Context())
	if err == nil {
		_, err = srv.Pause(r.Context(), pauseWait) // held: serve ends paused
	}
	if err != nil {
		_ = os.Remove(archive) // #nosec G703 -- the file receiveArchive named in our data directory
		return "", err
	}
	return archive, nil
}

// waitExports waits for the exports streaming to end.
func (im *importing) waitExports(ctx context.Context) error {
	deadline := time.Now().Add(pauseWait)
	for {
		im.mu.Lock()
		n := im.exports
		im.mu.Unlock()
		switch {
		case n == 0:
			return nil
		case time.Now().After(deadline):
			return errors.New(ErrUsage, "an export is still being downloaded: try again when it ends")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// receiveArchive saves the body into the data directory and checks it.
func (c *cli) receiveArchive(w http.ResponseWriter, r *http.Request) (string, error) {
	data, err := c.dataDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(data, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "import-"+strings.ToLower(rand.Text()[:8])+".tar.gz")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- our own data directory
	if err != nil {
		return "", err
	}
	// An archive of gigabytes takes longer than the server's read bound.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(time.Hour))
	_, err = io.Copy(f, http.MaxBytesReader(w, r.Body, backup.MaxTotal))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = verifyFile(r.Context(), path)
	}
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// pendingImport applies the import the app asked for, once serve has
// ended; true when there was one, and serve is to start again.
func (c *cli) pendingImport() bool {
	if c.imports == nil {
		return false
	}
	c.imports.mu.Lock()
	archive, ctx := c.imports.archive, c.imports.ctx
	c.imports.mu.Unlock()
	if archive == "" || ctx == nil {
		return false
	}
	defer func() { _ = os.Remove(archive) }() // #nosec G703 -- the file receiveArchive named in our data directory
	before, n, err := c.importState(ctx, archive)
	result := importResult{At: time.Now().UTC(), OK: err == nil, Files: n, Before: before}
	if err != nil {
		result.Error = describe(err)
		c.note("the import failed: " + result.Error)
	} else {
		c.note("imported " + strconv.Itoa(n) + " files; the state before is in " + clean(before) + "; starting again")
	}
	c.saveImportResult(result)
	return true
}

// importResult is how the last import from the app went, for the app to
// read once serve is back.
type importResult struct {
	At     time.Time `json:"at"`
	OK     bool      `json:"ok"`
	Files  int       `json:"files,omitempty"`
	Before string    `json:"before,omitempty"` // the copy of the state made first
	Error  string    `json:"error,omitempty"`
}

func (c *cli) importResultPath() (string, error) {
	data, err := c.dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(data, "backups", "last-import.json"), nil
}

func (c *cli) saveImportResult(r importResult) {
	path, err := c.importResultPath()
	if err != nil {
		return
	}
	if data, err := json.Marshal(r); err == nil {
		_ = os.WriteFile(path, data, 0o600) // #nosec G703 -- our own data directory
	}
}

func (c *cli) lastImport(w http.ResponseWriter, _ *http.Request) {
	path, err := c.importResultPath()
	if err == nil {
		var data []byte
		if data, err = os.ReadFile(path); err == nil { // #nosec G304 -- our own data directory
			var r importResult
			if json.Unmarshal(data, &r) == nil {
				writeJSON(w, http.StatusOK, r)
				return
			}
		}
	}
	fail(w, http.StatusNotFound, "STATE_NO_IMPORT", "no import from the app yet")
}
