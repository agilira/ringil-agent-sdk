// images.go: the images a request may carry
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package anthropic

import (
	"strconv"

	agent "github.com/agilira/ringil-agent-sdk"
)

// The API takes at most 100 images in a request, and at most 2000 pixels
// a side for each when there are more than 20: a conversation of
// screenshots would be refused at every turn. The latest maxImages are
// sent, at least imageStep fewer when it trims, so that the older images
// leave in steps and the turns before stay the same for the prompt cache
// (audit 21/11). A request is also at most 32 MB, base64 inside, and
// carries at most agent.MaxDocumentPages PDF pages: past maxAttached
// bytes or those pages the oldest go too, or every turn after would be
// refused.
const (
	maxImages   = 20
	imageStep   = 10
	maxAttached = 22 << 20 // 29 MB in base64, room left for the text
)

// fitImages is msgs with all but the latest images replaced by a note,
// when there are more than maxImages, or they are too large together.
func fitImages(msgs []agent.Message) []agent.Message {
	sizes, pages := attached(msgs)
	total := len(sizes)
	drop := 0
	if total > maxImages {
		keep := total - (total-maxImages+imageStep-1)/imageStep*imageStep
		drop = total - keep
	}
	drop = max(drop, overBudget(sizes, pages))
	if drop == 0 {
		return msgs
	}
	out := append([]agent.Message(nil), msgs...)
	for i := 0; i < len(out) && drop > 0; i++ {
		if n := out[i].Images(); n > 0 {
			var dropped int
			out[i], dropped = withoutImages(out[i], drop)
			drop -= dropped
		}
	}
	return out
}

// attached are the bytes and PDF pages of each image and document of
// msgs, oldest first, in the order withoutImages takes them.
func attached(msgs []agent.Message) (sizes, pages []int) {
	add := func(p agent.Part) {
		sizes = append(sizes, len(p.Data))
		if p.Type == agent.PartDocument {
			pages = append(pages, agent.DocumentPages(p.Data))
		} else {
			pages = append(pages, 0)
		}
	}
	for _, m := range msgs {
		if m.Result != nil {
			for _, p := range m.Result.Images {
				add(p)
			}
		}
		for _, p := range m.Parts {
			if p.Type == agent.PartImage || p.Type == agent.PartDocument {
				add(p)
			}
		}
	}
	return sizes, pages
}

// overBudget is how many of the oldest must go for the rest to fit in
// maxAttached bytes and agent.MaxDocumentPages pages.
func overBudget(sizes, pages []int) int {
	bytes, pdfPages := 0, 0
	for i := range sizes {
		bytes, pdfPages = bytes+sizes[i], pdfPages+pages[i]
	}
	drop := 0
	for drop < len(sizes) && (bytes > maxAttached || pdfPages > agent.MaxDocumentPages) {
		bytes, pdfPages = bytes-sizes[drop], pdfPages-pages[drop]
		drop++
	}
	return drop
}

// withoutImages takes up to n images out of m, oldest first, leaving a
// note; it returns how many it took.
func withoutImages(m agent.Message, n int) (agent.Message, int) {
	taken := 0
	if m.Result != nil && len(m.Result.Images) > 0 {
		res := *m.Result
		taken = min(n, len(res.Images))
		res.Images = res.Images[taken:]
		res.Content += "\n[" + plural(taken) + " the tool returned here no longer shown]"
		m.Result = &res
	}
	parts, gone := make([]agent.Part, 0, len(m.Parts)+1), 0
	for _, part := range m.Parts {
		if (part.Type == agent.PartImage || part.Type == agent.PartDocument) && taken < n {
			taken, gone = taken+1, gone+1
			continue
		}
		parts = append(parts, part)
	}
	if gone > 0 {
		m.Parts = append(parts, agent.Part{Type: agent.PartText, Text: "\n[" + plural(gone) + " sent here no longer shown]"})
	}
	return m, taken
}

func plural(n int) string {
	if n == 1 {
		return "1 image or document"
	}
	return strconv.Itoa(n) + " images or documents"
}
