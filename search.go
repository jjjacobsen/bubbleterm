package main

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type historySearchBlock struct {
	text       string
	from       int // skip rendered sender, reaction, or link labels
	first, end int // wrapped row range
}

type searchHit struct {
	id, part, offset int // stable across wrapping and refreshes
	first, last      int // chat index or wrapped history row range
}

type searchSpan struct {
	start, end int
	hit        int
}

type paneSearch struct {
	query       []rune
	cursor, top int
	active      int
	hits        []searchHit
	spans       map[int][]searchSpan
	chatLabels  map[int]string
}

func (m *ui) searchingChats() bool {
	return m.focus == uiChats || m.focus == uiHidden
}

func (m *ui) startSearch() {
	m.search = &paneSearch{active: -1, spans: make(map[int][]searchSpan), chatLabels: make(map[int]string)}
}

func (m *ui) searchKey(k tea.KeyMsg) tea.Cmd {
	s := m.search
	switch k.Type {
	case tea.KeyEsc:
		m.search = nil
	case tea.KeyEnter:
		m.search = nil
		if s.active >= 0 {
			hit := s.hits[s.active]
			switch m.focus {
			case uiHistory:
				m.rowTop, m.rowIndex = s.top, hit.first
			case uiHidden:
				m.hiddenTop, m.hiddenIndex = s.top, hit.first
			case uiChats:
				m.chatTop = s.top
				if hit.first != m.chatIndex {
					m.chatIndex = hit.first
					return m.resetHistory()
				}
			}
		}
	case tea.KeyCtrlJ, tea.KeyCtrlK:
		if n := len(s.hits); n > 0 && !k.Alt {
			delta := 1
			if k.Type == tea.KeyCtrlK {
				delta = -1
			}
			s.active = (s.active + delta + n) % n
		}
	case tea.KeyLeft:
		s.cursor = max(0, s.cursor-1)
	case tea.KeyRight:
		s.cursor = min(len(s.query), s.cursor+1)
	case tea.KeyHome:
		s.cursor = 0
	case tea.KeyEnd:
		s.cursor = len(s.query)
	case tea.KeyBackspace:
		if s.cursor > 0 {
			s.query = slices.Delete(s.query, s.cursor-1, s.cursor)
			s.cursor--
			m.updateSearch()
		}
	case tea.KeyDelete:
		if s.cursor < len(s.query) {
			s.query = slices.Delete(s.query, s.cursor, s.cursor+1)
			m.updateSearch()
		}
	case tea.KeyRunes, tea.KeySpace:
		if !k.Alt || k.Paste {
			text := []rune(oneLine(string(k.Runes)))
			s.query = slices.Insert(s.query, s.cursor, text...)
			s.cursor += len(text)
			m.updateSearch()
		}
	}
	return nil
}

func (m *ui) updateSearch() {
	s := m.search
	var previous searchHit
	hadHit := s.active >= 0
	if hadHit {
		previous = s.hits[s.active]
	}
	s.active, s.hits = -1, s.hits[:0]
	clear(s.spans)
	clear(s.chatLabels)
	if len(s.query) == 0 {
		return
	}
	pattern := regexp.MustCompile("(?i)" + regexp.QuoteMeta(string(s.query)))
	index := m.rowIndex
	switch m.focus {
	case uiChats:
		index = m.chatIndex
		m.matchChats(pattern, m.chats)
	case uiHidden:
		index = m.hiddenIndex
		m.matchChats(pattern, m.hidden.chats)
	default:
		m.matchHistory(pattern)
	}
	if len(s.hits) == 0 {
		return
	}
	if hadHit {
		s.active = slices.IndexFunc(s.hits, func(hit searchHit) bool {
			return hit.id == previous.id && hit.part == previous.part && hit.offset == previous.offset
		})
	}
	if s.active < 0 {
		s.active = max(0, slices.IndexFunc(s.hits, func(hit searchHit) bool { return hit.last >= index }))
	}
}

func (m *ui) matchChats(pattern *regexp.Regexp, chats []chat) {
	s := m.search
	for i, c := range chats {
		name := chatName(c)
		for _, field := range []string{name, oneLine(c.Name), oneLine(c.Identifier), strconv.Itoa(c.ID)} {
			matches := pattern.FindAllStringIndex(field, -1)
			if len(matches) == 0 {
				continue
			}
			prefix := ""
			if field != name {
				prefix = name + " · "
			}
			s.chatLabels[i] = prefix + field
			for _, span := range matches {
				s.spans[i] = append(s.spans[i], searchSpan{len(prefix) + span[0], len(prefix) + span[1], len(s.hits)})
			}
			s.hits = append(s.hits, searchHit{id: c.ID, first: i, last: i})
			break
		}
	}
}

func (m *ui) matchHistory(pattern *regexp.Regexp) {
	s := m.search
	for _, block := range m.historySearch {
		row := block.first
		for _, span := range pattern.FindAllStringIndex(block.text[block.from:], -1) {
			start, end := block.from+span[0], block.from+span[1]
			first, last := -1, -1
			for ; row < block.end; row++ {
				line := m.rows[row]
				if line.searchStart < 0 || line.searchStart+len(line.text) <= start {
					continue
				}
				if line.searchStart >= end {
					break
				}
				if line.text != "" {
					if first < 0 {
						first = row
					}
					last = row
					left, right := max(0, start-line.searchStart), min(len(line.text), end-line.searchStart)
					s.spans[row] = append(s.spans[row], searchSpan{left, right, len(s.hits)})
				}
				if end <= line.searchStart+len(line.text) {
					break
				}
			}
			if first >= 0 {
				line := m.rows[first]
				s.hits = append(s.hits, searchHit{m.messages[line.messageIndex].ID, line.linkNumber, span[0], first, last})
			}
		}
	}
}

func (m *ui) keepSearchVisible(height int) {
	s := m.search
	if s.active < 0 {
		s.top = m.rowTop
		switch m.focus {
		case uiChats:
			s.top = m.chatTop
		case uiHidden:
			s.top = m.hiddenTop
		}
		return
	}
	hit := s.hits[s.active]
	switch m.focus {
	case uiChats:
		s.top = m.scrollChats(s.top, hit.first, height)
	case uiHidden:
		s.top = scrollTop(s.top, hit.first, height, len(m.hidden.chats))
	default:
		s.top = scrollTop(s.top, hit.first, height, len(m.rows))
		if hit.last-hit.first < height {
			s.top = max(s.top, hit.last-height+1)
		}
	}
}

func (m *ui) searchSelection(index, top int) (int, int) {
	if s := m.search; s != nil && s.active >= 0 {
		return s.hits[s.active].first, s.top
	}
	return index, top
}

func (s *paneSearch) highlight(row int, text string, width int) string {
	spans := s.spans[row]
	if len(spans) == 0 || width <= 0 {
		return ansi.Truncate(text, width, "")
	}
	focus := spans[0]
	for _, span := range spans {
		if span.hit == s.active {
			focus = span
			break
		}
	}
	left := 0
	if ansi.StringWidth(text[:focus.end]) > width {
		prefix := text[:focus.start]
		context := max(0, width-ansi.StringWidth(text[focus.start:focus.end]))
		left = focus.start - len(ansi.TruncateLeft(prefix, max(0, ansi.StringWidth(prefix)-context), ""))
	}
	visible := ansi.Truncate(text[left:], width, "")
	right := left + len(visible)
	var out strings.Builder
	for _, span := range spans {
		start, end := max(left, span.start), min(right, span.end)
		if start >= end {
			continue
		}
		out.WriteString(text[left:start])
		if span.hit == s.active {
			out.WriteString("\x1b[1;7m" + text[start:end] + "\x1b[22;27m")
		} else {
			out.WriteString("\x1b[4m" + text[start:end] + "\x1b[24m")
		}
		left = end
	}
	out.WriteString(text[left:right])
	return out.String()
}

func (s *paneSearch) prompt(width int) string {
	count := fmt.Sprintf("  %d/%d", s.active+1, len(s.hits))
	if len(count)+3 > width {
		count = ""
	}
	width = max(0, width-len(count)-2) // slash and cursor
	before, after := string(s.query[:s.cursor]), string(s.query[s.cursor:])
	before = ansi.TruncateLeft(before, max(0, ansi.StringWidth(before)-width), "")
	after = ansi.Truncate(after, width-ansi.StringWidth(before), "")
	return "/" + before + "▏" + after + count
}
