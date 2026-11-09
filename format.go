package main

import (
	"cmp"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/go-telegram/bot/models"
)

// maxTextLimit is the limit of the MAX text field. MAX counts it over the raw
// text including HTML markup, in UTF-16 code units.
const maxTextLimit = 4000

// richText is Telegram text with its markup. Telegram counts entity offsets in
// UTF-16 code units, so the text is stored in them too.
type richText struct {
	units    []uint16
	entities []models.MessageEntity
}

func newRichText(text string, entities []models.MessageEntity) richText {
	return richText{units: utf16.Encode([]rune(text)), entities: entities}
}

// linkText is a line that is a link, as if it were formatted in Telegram.
func linkText(label, url string) richText {
	t := newRichText(label, nil)
	t.entities = []models.MessageEntity{{Type: models.MessageEntityTypeTextLink, Length: len(t.units), URL: url}}

	return t
}

// clone returns a copy that can be appended to without changing the original.
func (t richText) clone() richText {
	return richText{units: slices.Clone(t.units), entities: slices.Clone(t.entities)}
}

// append adds text after a blank line, shifting its entities.
func (t *richText) append(other richText) {
	if len(other.units) == 0 {
		return
	}
	if len(t.units) > 0 {
		t.units = append(t.units, '\n', '\n')
	}
	shift := len(t.units)
	for _, e := range other.entities {
		e.Offset += shift
		t.entities = append(t.entities, e)
	}
	t.units = append(t.units, other.units...)
}

// tags returns the MAX HTML tags for a Telegram entity. Entities MAX has no
// counterpart for (spoiler, mention by id, custom emoji, hashtag) stay plain text.
func (t richText) tags(e models.MessageEntity) (open, close string, ok bool) {
	switch e.Type {
	case models.MessageEntityTypeBold:
		return "<b>", "</b>", true
	case models.MessageEntityTypeItalic:
		return "<i>", "</i>", true
	case models.MessageEntityTypeUnderline:
		return "<ins>", "</ins>", true
	case models.MessageEntityTypeStrikethrough:
		return "<del>", "</del>", true
	case models.MessageEntityTypeCode, models.MessageEntityTypePre:
		return "<code>", "</code>", true
	case models.MessageEntityTypeBlockquote, models.MessageEntityTypeExpandableBlockquote:
		return "<blockquote>", "</blockquote>", true
	case models.MessageEntityTypeTextLink, models.MessageEntityTypeURL:
		href := t.linkURL(e)
		if href == "" {
			return "", "", false
		}

		return `<a href="` + attrEscaper.Replace(href) + `">`, "</a>", true
	}

	return "", "", false
}

func (t richText) linkURL(e models.MessageEntity) string {
	if e.Type == models.MessageEntityTypeTextLink {
		return e.URL
	}
	if e.Type != models.MessageEntityTypeURL || e.Offset < 0 || e.Length <= 0 || e.Offset > len(t.units) || e.Length > len(t.units)-e.Offset {
		return ""
	}
	href := string(utf16.Decode(t.units[e.Offset : e.Offset+e.Length]))
	if !strings.Contains(href, "://") {
		href = "https://" + href
	}
	return href
}

var (
	textEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	attrEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
)

type span struct {
	start, end  int
	open, close string
}

// render turns the range [from, to) into MAX HTML. Entities crossing the range
// borders are cut at them, so every part of a long post has closed markup.
func (t richText) render(from, to int) string {
	var spans []span
	for _, e := range t.entities {
		open, close, ok := t.tags(e)
		start, end := max(e.Offset, from), min(e.Offset+e.Length, to)
		if !ok || start >= end {
			continue
		}
		spans = append(spans, span{start, end, open, close})
	}
	// An outer entity opens before a nested one
	slices.SortStableFunc(spans, func(a, b span) int {
		if a.start != b.start {
			return cmp.Compare(a.start, b.start)
		}

		return cmp.Compare(b.end, a.end)
	})

	var (
		out   strings.Builder
		run   []uint16
		stack []span
		next  int
	)
	flush := func() {
		out.WriteString(textEscaper.Replace(string(utf16.Decode(run))))
		run = run[:0]
	}
	// Closes the entities that ended by pos. Telegram does not let entities
	// overlap partially, but if that happens, the extra tags are reopened.
	closeEnded := func(pos int) {
		first := -1
		for i, s := range stack {
			if s.end <= pos {
				first = i

				break
			}
		}
		if first < 0 {
			return
		}
		flush()
		for i := len(stack) - 1; i >= first; i-- {
			out.WriteString(stack[i].close)
		}
		rest := stack[first:]
		stack = stack[:first:first]
		for _, s := range rest {
			if s.end > pos {
				out.WriteString(s.open)
				stack = append(stack, s)
			}
		}
	}

	for pos := from; pos < to; pos++ {
		closeEnded(pos)
		for ; next < len(spans) && spans[next].start == pos; next++ {
			flush()
			out.WriteString(spans[next].open)
			stack = append(stack, spans[next])
		}
		run = append(run, t.units[pos])
	}
	flush()
	closeEnded(to)

	return out.String()
}

// split cuts the text into parts that each fit into limit as HTML. A border
// goes at a blank line, then at a line break, then at a space, and only as a
// last resort in the middle of a word.
func (t richText) split(limit int) []string {
	t = t.clone()
	// Preserve an address as visible text if its escaped href cannot fit
	// even beside one character, or exceeds MAX's link length limit.
	var kept []models.MessageEntity
	var addresses []string
	for _, e := range t.entities {
		open, close, ok := t.tags(e)
		if ok && (htmlLen(open+close)+5 > limit || htmlLen(t.linkURL(e)) > 2048) {
			if e.Type == models.MessageEntityTypeTextLink && !slices.Contains(addresses, e.URL) {
				addresses = append(addresses, e.URL)
			}
			continue
		}
		kept = append(kept, e)
	}
	t.entities = kept
	for _, address := range addresses {
		t.append(newRichText(address, nil))
	}
	var parts []string
	end := trimRight(t.units, len(t.units))
	for from := skipSpace(t.units, 0); from < end; {
		first := from + 1
		if t.units[from] >= 0xD800 && t.units[from] < 0xDC00 && first < end {
			first++
		}
		if htmlLen(t.render(from, first)) > limit {
			// Several nested entities may together exceed the budget.
			// Drop only the entities active here, keeping their link targets.
			kept = nil
			for _, e := range t.entities {
				if e.Offset <= from && e.Offset+e.Length > from {
					if e.Type == models.MessageEntityTypeTextLink && !slices.Contains(addresses, e.URL) {
						addresses = append(addresses, e.URL)
						t.append(newRichText(e.URL, nil))
					}
					continue
				}
				kept = append(kept, e)
			}
			t.entities = kept
			end = trimRight(t.units, len(t.units))
		}
		to := end
		if htmlLen(t.render(from, to)) > limit {
			to = t.cut(from, end, limit)
		}
		parts = append(parts, t.render(from, trimRight(t.units, to)))
		from = skipSpace(t.units, to)
	}

	return parts
}

// cut finds the farthest border of the part starting at from.
func (t richText) cut(from, end, limit int) int {
	fits := func(to int) bool {
		return htmlLen(t.render(from, trimRight(t.units, to))) <= limit
	}

	for _, sep := range [][]uint16{{'\n', '\n'}, {'\n'}, {' '}} {
		best := -1
		for i := from + 1; i+len(sep) <= end; i++ {
			if !hasPrefix(t.units[i:], sep) {
				continue
			}
			// HTML length grows with the part: the first border that does not fit ends the search
			if !fits(i) {
				break
			}
			best = i
		}
		if best > from {
			return best
		}
	}

	lo, hi := from+1, end
	if t.units[from] >= 0xD800 && t.units[from] < 0xDC00 && lo < end {
		lo++
	}
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if fits(mid) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	// Do not cut a surrogate pair (an emoji) in two
	if lo-1 > from && utf16.IsSurrogate(rune(t.units[lo-1])) && t.units[lo-1] < 0xDC00 {
		lo--
	}

	return lo
}

// htmlLen is the length as MAX counts it: in UTF-16 code units.
func htmlLen(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}

	return n
}

func isSpace(u uint16) bool {
	return u == ' ' || u == '\n' || u == '\t' || u == '\r'
}

func skipSpace(units []uint16, i int) int {
	for i < len(units) && isSpace(units[i]) {
		i++
	}

	return i
}

func trimRight(units []uint16, i int) int {
	for i > 0 && isSpace(units[i-1]) {
		i--
	}

	return i
}

func hasPrefix(units, prefix []uint16) bool {
	if len(units) < len(prefix) {
		return false
	}
	for i, u := range prefix {
		if units[i] != u {
			return false
		}
	}

	return true
}
