// compact.go: fitting a long conversation into the model's context
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"sync"
)

// Compaction controls how a conversation that outgrows the model's context
// is shortened. It changes only what is sent to the model: the conversation
// itself keeps every message.
//
// First, old tool results are replaced by a one-line stub (size and hash);
// the most recent ones stay whole. If that is not enough, the oldest turns
// are left out, or summarized by the model when Summarize is set. Enough is
// cut to leave room for several more turns, so a growing chat is not
// re-compacted, and re-summarized, on every turn.
//
// Compaction starts when the estimated input plus MaxTokens passes
// Threshold of the context window, and again, harder, when the provider
// reports the context is full.
type Compaction struct {
	// Disabled sends the full conversation every time.
	Disabled bool
	// Threshold is the fraction of the context window that starts
	// compaction; values outside (0, 1] mean 0.8.
	Threshold float64
	// KeepResults is how many recent tool results stay whole; zero means 4.
	KeepResults int
	// Summarize replaces left-out turns with a summary written by the
	// model. A summary costs one model call; it is extended, not rewritten,
	// when more turns are left out later. If summarizing fails, the turns
	// are left out with a plain note instead.
	Summarize bool
	// SummaryMaxTokens caps the summary's length; zero means 1024.
	SummaryMaxTokens int
}

const (
	defaultThreshold   = 0.8
	defaultKeepResults = 4
	defaultSummaryMax  = 1024
	// stubMinBytes: results shorter than this are kept, a stub would not
	// save anything.
	stubMinBytes = 256
	// bytesPerToken is the estimate used before the provider has reported
	// any usage; afterwards the ratio is measured, within bounds, so a
	// provider reporting nonsense cannot switch compaction off or on.
	bytesPerToken  = 4.0
	minBytesPerTok = 1.0
	maxBytesPerTok = 16.0
	// summaryCacheSize bounds the summaries kept by a long-lived agent.
	summaryCacheSize = 32
)

func (c Compaction) threshold() float64 {
	if !(c.Threshold > 0 && c.Threshold <= 1) {
		return defaultThreshold
	}
	return c.Threshold
}

func (c Compaction) keep() int {
	if c.KeepResults <= 0 {
		return defaultKeepResults
	}
	return c.KeepResults
}

func (c Compaction) summaryMax() int {
	if c.SummaryMaxTokens <= 0 {
		return defaultSummaryMax
	}
	return c.SummaryMaxTokens
}

// compactor builds the view of a conversation sent to the model. One
// compactor serves every run of an agent: the learned bytes-per-token ratio
// and the summaries carry over from turn to turn.
type compactor struct {
	cfg      Compaction
	window   int
	provider Provider
	history  bool // read_history is offered: the notes point to it

	mu        sync.Mutex
	ratio     float64
	summaries []cachedSummary
}

// unlimited is the budget when the window is unknown: only the provider's
// refusal can then trigger compaction.
const unlimited = math.MaxInt

// budget is the input token count that starts compaction.
func (c *compactor) budget(maxTokens int) int {
	if c.window <= 0 {
		return unlimited
	}
	return max(1, int(float64(c.window)*c.cfg.threshold())-maxTokens)
}

func (c *compactor) tokens(bytes int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	ratio := c.ratio
	if ratio == 0 {
		ratio = bytesPerToken
	}
	return int(float64(bytes) / ratio)
}

func (c *compactor) learn(bytes, tokens int) {
	if bytes <= 0 || tokens <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ratio = min(max(float64(bytes)/float64(tokens), minBytesPerTok), maxBytesPerTok)
}

func requestBytes(req *Request) int {
	data, err := json.Marshal(req)
	if err != nil {
		return 0 // unmeasurable: the retry then compacts as hard as it can
	}
	return len(data)
}

// compacted is a view and what it took to build it.
type compacted struct {
	msgs  []Message
	what  string
	usage Usage
}

// view returns the messages to send within budget tokens.
func (c *compactor) view(ctx context.Context, req *Request, budget int) compacted {
	if budget == unlimited {
		return compacted{msgs: req.Messages}
	}
	v := newSized(req, c)
	if v.fits(budget) {
		return compacted{msgs: req.Messages}
	}
	stubbed := v.stub(c.cfg.keep())
	out := compacted{}
	if !v.fits(budget) {
		out = c.shorten(ctx, v, budget)
	}
	if !v.fits(budget) {
		// One turn alone is too big: keep only the newest result whole.
		stubbed += v.stub(1)
	}
	out.msgs = v.msgs
	out.what = joinNonEmpty(describeStubs(stubbed), out.what)
	return out
}

func describeStubs(n int) string {
	if n == 0 {
		return ""
	}
	return plural(n, "old tool result") + " shortened"
}

func joinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ", ")
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return strconv.Itoa(n) + " " + what + "s"
}

// shorten leaves out the oldest turns. The cut is chosen with a plain note,
// which costs nothing, keeping at most half the budget when possible so the
// next turns still fit; only then, if enabled, is the left-out part
// summarized.
func (c *compactor) shorten(ctx context.Context, v *sized, budget int) compacted {
	if out, ok := c.reuse(v, budget); ok {
		return out
	}
	cut := v.cut(budget, c.reserve())
	if cut == 0 {
		return compacted{}
	}
	note, usage, how := c.countNote(cut), Usage{}, "left out"
	if c.cfg.Summarize {
		if summary, used, err := c.summary(ctx, v.msgs[:cut], budget); err == nil {
			note, usage, how = summary, used, "summarized"
		} else {
			usage, how = used, "left out (summary failed)"
		}
	}
	v.drop(cut, note)
	return compacted{what: plural(cut, "earlier message") + " " + how, usage: usage}
}

// reuse applies the summary already made for the longest prefix of this
// conversation, when the rest still fits with it: a growing chat then pays
// for a new summary only when the old one stops being enough.
func (c *compactor) reuse(v *sized, budget int) (compacted, bool) {
	if !c.cfg.Summarize {
		return compacted{}, false
	}
	prev := c.lookup(prefixHashes(v.msgs))
	if prev.n == 0 || prev.n >= len(v.msgs) {
		return compacted{}, false
	}
	trial := v.clone()
	trial.drop(prev.n, prev.text)
	if !trial.fits(budget) {
		return compacted{}, false
	}
	*v = *trial
	return compacted{what: plural(prev.n, "earlier message") + " summarized"}, true
}

// reserve is the room, in bytes, kept for the note that replaces the cut.
func (c *compactor) reserve() int {
	if !c.cfg.Summarize {
		return 200
	}
	return int(float64(c.cfg.summaryMax()+100) * bytesPerToken)
}

func (c *compactor) countNote(n int) string {
	return "[" + plural(n, "earlier message") + " of this conversation omitted to fit the context (messages 1-" + strconv.Itoa(n) + c.hint("; ", " shows them") + ").]"
}

// hint names read_history only when it is there to call.
func (c *compactor) hint(before, after string) string {
	if !c.history {
		return ""
	}
	return before + HistoryToolName + after
}

// sized is a view under construction with the byte size of every message,
// so trying cuts does not re-encode the whole conversation each time.
type sized struct {
	c       *compactor
	base    int
	msgs    []Message
	sizes   []int
	total   int
	offset  int          // messages dropped from the front: msgs[i] is message offset+i+1
	stubbed map[int]bool // positions already stubbed in this view
}

func newSized(req *Request, c *compactor) *sized {
	probe := *req
	probe.Messages = nil
	v := &sized{c: c, base: requestBytes(&probe), msgs: append([]Message(nil), req.Messages...)}
	v.sizes = make([]int, len(v.msgs))
	for i := range v.msgs {
		v.resize(i)
	}
	return v
}

func (v *sized) clone() *sized {
	c := *v
	c.msgs = append([]Message(nil), v.msgs...)
	c.sizes = append([]int(nil), v.sizes...)
	return &c
}

func messageBytes(msg Message) int {
	data, err := json.Marshal(msg)
	if err != nil {
		return 0
	}
	return len(data) + 1
}

func (v *sized) resize(i int) {
	v.total -= v.sizes[i]
	v.sizes[i] = messageBytes(v.msgs[i])
	v.total += v.sizes[i]
}

func (v *sized) fits(budget int) bool {
	return v.c.tokens(v.base+v.total) <= budget
}

// stub replaces all but the last keep tool results with a stub.
func (v *sized) stub(keep int) int {
	seen, stubbed := 0, 0
	for i := len(v.msgs) - 1; i >= 0; i-- {
		m := v.msgs[i]
		if m.Role != RoleTool || m.Result == nil {
			continue
		}
		seen++
		if seen <= keep || len(m.Result.Content) < stubMinBytes || v.stubbed[v.offset+i] {
			continue
		}
		res := *m.Result
		res.Content = v.c.stub(res.Content, v.offset+i+1)
		if v.stubbed == nil {
			v.stubbed = map[int]bool{}
		}
		v.stubbed[v.offset+i] = true
		v.msgs[i] = ToolMessage(res)
		v.resize(i)
		stubbed++
	}
	return stubbed
}

const stubPrefix = "[Tool result omitted to fit the context: "

// stub stands in for a result; only this view's own stubs are skipped
// later (by position), so a result that merely looks like one is still
// shortened.
func (c *compactor) stub(content string, number int) string {
	return stubPrefix + strconv.Itoa(len(content)) + " bytes, sha256 " + shortHash(content) +
		". It is message " + strconv.Itoa(number) + c.hint(": ", " shows it, or") + " run a read-only tool again.]"
}

// cut returns where to cut so the rest fits half the budget, or else the
// whole budget, or else the last possible cut; zero when no cut exists. A
// cut always lands on a user message that starts a turn, so the view stays
// a valid conversation.
func (v *sized) cut(budget, reserve int) int {
	starts := cutPoints(v.msgs)
	if len(starts) == 0 {
		return 0
	}
	last := starts[len(starts)-1]
	kept, prev, fallback := v.total, 0, last
	for _, start := range starts {
		for i := prev; i < start; i++ {
			kept -= v.sizes[i]
		}
		prev = start
		size := v.c.tokens(v.base + kept + reserve)
		if size <= budget/2 {
			return start
		}
		if size <= budget && fallback == last {
			fallback = start
		}
	}
	return fallback
}

// cutPoints are the indexes, after the first message, where a turn starts:
// a user message not preceded by another user message.
func cutPoints(msgs []Message) []int {
	var points []int
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role == RoleUser && msgs[i-1].Role != RoleUser {
			points = append(points, i)
		}
	}
	return points
}

// drop removes the first cut messages and prepends note to the first kept
// message, as its own part.
func (v *sized) drop(cut int, note string) {
	v.offset += cut
	v.msgs = v.msgs[cut:]
	for _, s := range v.sizes[:cut] {
		v.total -= s
	}
	v.sizes = v.sizes[cut:]
	first := v.msgs[0].Clone()
	first.Parts = append([]Part{{Type: PartText, Text: note + "\n\n"}}, first.Parts...)
	v.msgs[0] = first
	v.resize(0)
}
