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
// (audit 21/11).
const (
	maxImages = 20
	imageStep = 10
)

// fitImages is msgs with all but the latest images replaced by a note,
// when there are more than maxImages.
func fitImages(msgs []agent.Message) []agent.Message {
	total := 0
	for _, m := range msgs {
		total += m.Images()
	}
	if total <= maxImages {
		return msgs
	}
	keep := total - (total-maxImages+imageStep-1)/imageStep*imageStep
	drop := total - keep
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
		if part.Type == agent.PartImage && taken < n {
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
		return "1 image"
	}
	return strconv.Itoa(n) + " images"
}
