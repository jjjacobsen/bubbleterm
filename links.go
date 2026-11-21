package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const linkBalloon = "com.apple.messages.URLBalloonProvider"

// Read the archived URL without unarchiving objects or returning preview assets.
const readLinkPreviews = `import json, pathlib, plistlib, sqlite3, sys


def destination(payload):
    archive = plistlib.loads(payload, fmt=plistlib.FMT_BINARY)
    objects = archive["$objects"]

    def value(ref):
        return objects[ref.data] if isinstance(ref, plistlib.UID) else ref

    root = value(archive["$top"]["root"])
    metadata = value(root["richLinkMetadata"])
    if not isinstance(metadata, dict):
        return None
    for key in ("originalURL", "URL"):
        link = value(metadata.get(key))
        if isinstance(link, dict):
            link = value(link.get("NS.relative"))
        if isinstance(link, str) and link.lower().startswith(("https://", "http://")):
            return link


balloon, *ids = sys.argv[1:]
path = (pathlib.Path.home() / "Library/Messages/chat.db").as_uri()
db = sqlite3.connect(path + "?mode=ro", uri=True)
query = "SELECT ROWID, payload_data FROM message WHERE balloon_bundle_id = ? AND payload_data IS NOT NULL AND ROWID IN (%s)"
links = {}
for mid, payload in db.execute(query % ",".join("?" for _ in ids), [balloon, *ids]):
    try:
        link = destination(payload)
    except (plistlib.InvalidFileException, KeyError, IndexError, TypeError):
        continue
    if link:
        links[mid] = link
print(json.dumps(links))
`

var textURL = regexp.MustCompile("(?i)https?://[^\\p{Z}\\p{C}<>\"`“”‘’\\x{FFFC}]+")

func validLink(link string) bool {
	u, err := url.Parse(link)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != ""
}

func trimLink(raw string) string {
	for {
		raw = strings.TrimRight(raw, ".,!?;:'")
		if raw == "" {
			return raw
		}
		last := raw[len(raw)-1:]
		start := strings.Index(")]}", last)
		if start < 0 || strings.Count(raw, "([{"[start:start+1]) >= strings.Count(raw, last) {
			return raw
		}
		raw = raw[:len(raw)-1]
	}
}

func messageLinks(text, preview string) []string {
	var links []string
	for _, raw := range textURL.FindAllString(text, -1) {
		link := trimLink(raw)
		// The archive disambiguates punctuation that belongs to the sent URL.
		if preview != "" && strings.HasPrefix(raw, preview) && link == trimLink(preview) {
			link = preview
		}
		if validLink(link) && !slices.Contains(links, link) {
			links = append(links, link)
		}
	}
	if validLink(preview) && !slices.Contains(links, preview) {
		links = append(links, preview)
	}
	return links
}

func (b backend) loadHistoryLinks(rows []message) error {
	var ids []string
	for _, row := range rows {
		if row.UnsentAt == nil && row.BalloonBundleID == linkBalloon {
			ids = append(ids, strconv.Itoa(row.ID))
		}
	}
	var previews map[int]string
	if len(ids) != 0 {
		command := imsgPath + "exec uv --no-config --offline run --no-project --no-managed-python --no-python-downloads --python /usr/bin/python3 python - " +
			shellQuote(linkBalloon) + " " + strings.Join(ids, " ") + " <<'PY'\n" + readLinkPreviews + "PY"
		out, err := b.read("read link previews", command)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(out, &previews); err != nil {
			return fmt.Errorf("decode link previews: %w", err)
		}
	}
	for i := range rows {
		row := &rows[i]
		if row.UnsentAt == nil {
			row.Links = messageLinks(row.Text, previews[row.ID])
		}
	}
	return nil
}
