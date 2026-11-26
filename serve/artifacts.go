// artifacts.go: what the agents made, for the app beside the chat
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package serve

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/agilira/go-errors"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/artifact"
)

// ArtifactVersions is an artifact with every version kept.
type ArtifactVersions struct {
	Latest   artifact.Info   `json:"latest"`
	Versions []artifact.Info `json:"versions"`
}

// inlineImages are the images whose bytes are shown as themselves: they
// run nothing. Everything else is text (shown by the app, an HTML page in
// a sandboxed frame of its own) or a download.
var inlineImages = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// artifactScope checks a conversation's ID as the scope of its artifacts.
func (s *Server) artifactScope(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	switch {
	case s.cfg.Artifacts == nil:
		failure(w, http.StatusNotFound, fail(ErrNotFound, "the server keeps no artifacts"))
	case agent.ValidateSessionID(id) != nil || !artifact.ValidScope(id):
		failure(w, http.StatusBadRequest, fail(ErrInput, "a conversation ID is 1-64 letters, digits, - and _"))
	case s.cfg.Store != nil && !s.exists(r.Context(), id):
		failure(w, http.StatusNotFound, fail(ErrNotFound, "no conversation "+id))
	default:
		return id, true
	}
	return "", false
}

func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.artifactScope(w, r)
	if !ok {
		return
	}
	list, err := s.cfg.Artifacts.List(r.Context(), scope)
	if err != nil {
		failure(w, artifactStatus(err), err)
		return
	}
	if list == nil {
		list = []artifact.Info{}
	}
	respond(w, http.StatusOK, list)
}

func (s *Server) artifactVersions(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.artifactScope(w, r)
	if !ok {
		return
	}
	versions, err := s.cfg.Artifacts.Versions(r.Context(), scope, r.PathValue("name"))
	if err != nil {
		failure(w, artifactStatus(err), err)
		return
	}
	if len(versions) == 0 {
		failure(w, http.StatusNotFound, fail(ErrNotFound, "no such artifact"))
		return
	}
	respond(w, http.StatusOK, ArtifactVersions{Latest: versions[len(versions)-1], Versions: versions})
}

// artifactContent answers a version's bytes: an image as itself, text as
// plain text, any other file as a download; never as a page of this
// origin (the sandbox makes even a direct visit a page of no origin).
func (s *Server) artifactContent(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.artifactScope(w, r)
	if !ok {
		return
	}
	version := 0
	if v := r.URL.Query().Get("version"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			failure(w, http.StatusBadRequest, fail(ErrInput, "version is a number from 1"))
			return
		}
		version = n
	}
	info, data, err := s.cfg.Artifacts.Get(r.Context(), scope, r.PathValue("name"), version)
	if err != nil {
		failure(w, artifactStatus(err), err)
		return
	}
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("X-Artifact-Version", strconv.Itoa(info.Version))
	switch {
	case inlineImages[info.MediaType]:
		h.Set("Content-Type", info.MediaType)
	case artifact.Textual(info.Kind):
		h.Set("Content-Type", "text/plain; charset=utf-8")
	default:
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", `attachment; filename="`+downloadName(info.Name)+`"`)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data) // #nosec G705 -- never a page: text/plain, a raster image or a download, nosniff, and a sandbox CSP
}

// downloadName is name as a header carries it: artifact names are
// letters, digits, . _ - already; anything else goes.
func downloadName(name string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == '"' || r == '\\' || r > 0x7e {
			return '_'
		}
		return r
	}, name)
}

func (s *Server) deleteArtifact(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.artifactScope(w, r)
	if !ok {
		return
	}
	if err := s.cfg.Artifacts.Delete(r.Context(), scope, r.PathValue("name")); err != nil {
		failure(w, artifactStatus(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func artifactStatus(err error) int {
	switch {
	case errors.HasCode(err, artifact.ErrNotFound):
		return http.StatusNotFound
	case errors.HasCode(err, artifact.ErrInput):
		return http.StatusBadRequest
	}
	return status(err)
}
