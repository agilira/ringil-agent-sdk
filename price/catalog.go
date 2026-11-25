// catalog.go: the prices downloaded and kept on disk
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package price

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agilira/go-errors"

	"github.com/agilira/ringil-agent-sdk/dirs"
	"github.com/agilira/ringil-agent-sdk/internal/atomicfile"
	"github.com/agilira/ringil-agent-sdk/internal/safehttp"
)

// ErrCatalog: the prices could not be downloaded or read.
const ErrCatalog errors.ErrorCode = "PRICE_CATALOG"

// OpenRouterURL is OpenRouter's public list of models, with their prices
// in dollars per token. It needs no key.
const OpenRouterURL = "https://openrouter.ai/api/v1/models"

// maxCatalog bounds the download: the list is about 1 MB.
const maxCatalog = 32 << 20

// Catalog keeps a downloaded price list on disk and downloads it again
// when it is older than MaxAge.
type Catalog struct {
	// Path is the file; default <state>/prices.json of the program's
	// places (dirs.Program).
	Path string
	// MaxAge is how long a download is used; default 24 hours.
	MaxAge time.Duration
	// URL is the list, in OpenRouter's format; default OpenRouterURL.
	URL string
	// Client makes the request; default http.DefaultClient. Every client
	// is limited to HTTPS and to the size of a list.
	Client *http.Client
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// saved is the file: the table and when it was downloaded.
type saved struct {
	Fetched time.Time `json:"fetched"`
	Source  string    `json:"source"`
	Models  Table     `json:"models"`
}

// Load returns the prices: the file while it is newer than MaxAge, else a
// new download, saved. When the download fails and the file is there,
// its table comes back with the error: old prices are better than none,
// and the host says so if it wants.
func (c *Catalog) Load(ctx context.Context) (Table, error) {
	path, err := c.path()
	if err != nil {
		return nil, err
	}
	old, readErr := readSaved(path)
	if readErr == nil && c.now().Sub(old.Fetched) < c.maxAge() && old.Source == c.url() {
		return old.Models, nil
	}
	t, err := c.Fetch(ctx)
	if err != nil {
		if readErr == nil {
			return old.Models, err
		}
		return nil, err
	}
	if err := writeSaved(path, saved{Fetched: c.now(), Source: c.url(), Models: t}); err != nil {
		return t, err
	}
	return t, nil
}

// Fetch downloads the list and returns its table, without the file.
func (c *Catalog) Fetch(ctx context.Context) (Table, error) {
	if err := safehttp.CheckURL(c.url(), false); err != nil {
		return nil, errors.Wrap(err, ErrCatalog, "the price list must be an HTTPS address")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(), nil)
	if err != nil {
		return nil, errors.Wrap(err, ErrCatalog, "bad price list address")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := safehttp.Client(c.Client, nil, maxCatalog, false).Do(req) // #nosec G704 -- an HTTPS address the host set, checked above
	if err != nil {
		return nil, errors.Wrap(err, ErrCatalog, "the price list could not be downloaded")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(ErrCatalog, "the price list answered "+strconv.Itoa(resp.StatusCode))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, ErrCatalog, "the price list could not be read")
	}
	return Parse(body)
}

// wireModel is a model of OpenRouter's list; prices are dollars per
// token, as text.
type wireModel struct {
	ID      string      `json:"id"`
	Pricing wirePricing `json:"pricing"`
}

type wirePricing struct {
	Prompt     string        `json:"prompt"`
	Completion string        `json:"completion"`
	CacheRead  string        `json:"input_cache_read"`
	CacheWrite string        `json:"input_cache_write"`
	MinPrompt  int           `json:"min_prompt_tokens"`
	Overrides  []wirePricing `json:"overrides"`
}

// Parse reads a list in OpenRouter's format. A model is stored by its
// full name ("x-ai/grok-4-7") and by its own ("grok-4-7"), in the normal
// form of Lookup; an own name two vendors share at different prices is
// left out, never guessed. Variants (":free", ":batch") and models
// without a fixed price (routers) are left out.
func Parse(body []byte) (Table, error) {
	var list struct {
		Data []wireModel `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, errors.Wrap(err, ErrCatalog, "the price list is not in the expected format")
	}
	t, shared := Table{}, map[string]bool{}
	for _, m := range list.Data {
		t.add(m, shared)
	}
	if len(t) == 0 {
		return nil, errors.New(ErrCatalog, "the price list has no models")
	}
	return t, nil
}

// add stores m under its full and its own name; shared holds the own
// names two vendors give different prices.
func (t Table) add(m wireModel, shared map[string]bool) {
	if m.ID == "" || strings.Contains(m.ID, ":") {
		return
	}
	p, ok := m.Pricing.price()
	if !ok {
		return
	}
	t[normal(m.ID)] = p
	own := normal(m.ID[strings.LastIndexByte(m.ID, '/')+1:])
	if q, seen := t[own]; seen && !same(p, q) || shared[own] {
		shared[own] = true
		delete(t, own)
		return
	}
	t[own] = p
}

func same(a, b Price) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// price converts dollars per token to dollars per million, with the
// longer prompts' tiers. A cache price the list lacks is the input price.
func (w wirePricing) price() (Price, bool) {
	in, ok1 := perMillion(w.Prompt)
	out, ok2 := perMillion(w.Completion)
	if !ok1 || !ok2 {
		return Price{}, false
	}
	p := Price{Input: in, Output: out, CacheRead: in, CacheWrite: in}
	if v, ok := perMillion(w.CacheRead); ok {
		p.CacheRead = v
	}
	if v, ok := perMillion(w.CacheWrite); ok {
		p.CacheWrite = v
	}
	tiers := append([]wirePricing(nil), w.Overrides...)
	sort.Slice(tiers, func(i, j int) bool { return tiers[i].MinPrompt < tiers[j].MinPrompt })
	cur, from := &p, 0
	for _, tier := range tiers { // shortest first: each tier leads to the next
		if tier.MinPrompt <= from {
			continue
		}
		long, ok := tier.inherit(w).price()
		if !ok {
			continue
		}
		cur.LongFrom, cur.Long = tier.MinPrompt, &long
		cur, from = cur.Long, tier.MinPrompt
	}
	return p, true
}

// inherit fills a tier's missing prices from the base.
func (w wirePricing) inherit(base wirePricing) wirePricing {
	if w.Prompt == "" {
		w.Prompt = base.Prompt
	}
	if w.Completion == "" {
		w.Completion = base.Completion
	}
	w.Overrides = nil
	return w
}

// perMillion reads a price per token; a missing, negative or absurd one
// is not a price.
func perMillion(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) || v > 1 {
		return 0, false
	}
	return v * 1e6, true
}

func readSaved(path string) (saved, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return saved{}, err
	}
	defer func() { _ = root.Close() }() // #nosec G104 -- read only
	b, err := atomicfile.Read(root, filepath.Base(path), maxCatalog)
	if err != nil {
		return saved{}, err
	}
	var s saved
	if err := json.Unmarshal(b, &s); err != nil || !validTable(s.Models) {
		return saved{}, errors.New(ErrCatalog, "the saved price list is damaged")
	}
	return s, nil
}

func validTable(t Table) bool {
	if len(t) == 0 {
		return false
	}
	for _, p := range t {
		if !validPrice(p) {
			return false
		}
	}
	return true
}

// writeSaved replaces the file atomically: a reader never sees half of it.
func writeSaved(path string, s saved) error {
	b, err := json.Marshal(s)
	if err != nil {
		return errors.Wrap(err, ErrCatalog, "the price list could not be encoded")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.Wrap(err, ErrCatalog, "the price list could not be saved")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return errors.Wrap(err, ErrCatalog, "the price list could not be saved")
	}
	defer func() { _ = root.Close() }() // #nosec G104 -- Write already synced file and dir
	if err := atomicfile.Write(root, filepath.Base(path), b); err != nil {
		return errors.Wrap(err, ErrCatalog, "the price list could not be saved")
	}
	return nil
}

func (c *Catalog) path() (string, error) {
	if c.Path != "" {
		return c.Path, nil
	}
	d, err := dirs.Program()
	if err != nil {
		return "", errors.Wrap(err, ErrCatalog, "no place for the price list")
	}
	return filepath.Join(d.State, "prices.json"), nil
}

func (c *Catalog) maxAge() time.Duration {
	if c.MaxAge > 0 {
		return c.MaxAge
	}
	return 24 * time.Hour
}

func (c *Catalog) url() string {
	if c.URL != "" {
		return c.URL
	}
	return OpenRouterURL
}

func (c *Catalog) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
