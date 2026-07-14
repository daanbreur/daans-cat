package main

import (
	"encoding/xml"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"time"
)

type rss struct {
	XMLName   xml.Name `xml:"rss"`
	Version   string   `xml:"version,attr"`
	AtomNS    string   `xml:"xmlns:atom,attr"`
	MediaNS   string   `xml:"xmlns:media,attr"`
	ChannelEl channel  `xml:"channel"`
}

type channel struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	Language    string    `xml:"language"`
	Updated     string    `xml:"lastBuildDate,omitempty"`
	AtomLink    atomLink  `xml:"atom:link"`
	Items       []rssItem `xml:"item"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

type rssItem struct {
	Title       string     `xml:"title"`
	Link        string     `xml:"link"`
	GUID        guid       `xml:"guid"`
	PubDate     string     `xml:"pubDate"`
	Description string     `xml:"description"` // escaped HTML, as RSS wants
	Enclosure   enclosure  `xml:"enclosure"`
	MediaThumb  mediaThumb `xml:"media:thumbnail"`
}

type guid struct {
	Value       string `xml:",chardata"`
	IsPermaLink bool   `xml:"isPermaLink,attr"`
}

type enclosure struct {
	URL    string `xml:"url,attr"`
	Length int64  `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

type mediaThumb struct {
	URL string `xml:"url,attr"`
}

const feedLimit = 50

func (a *App) handleRSS(w http.ResponseWriter, r *http.Request) {
	posts := a.store.List()
	if len(posts) > feedLimit {
		posts = posts[:feedLimit]
	}

	items := make([]rssItem, 0, len(posts))
	for _, p := range posts {
		link := a.absURL("p", p.ID)
		img := a.absURL("media", p.Image)

		title := p.Caption
		if title == "" {
			title = p.Date.Format("January 2, 2006")
		}

		// The photo *is* the post, so put it in the body — readers that only
		// show descriptions still get the cat.
		body := fmt.Sprintf(`<p><img src="%s" alt="%s" width="%d" height="%d"></p>`,
			html.EscapeString(img), html.EscapeString(title), p.Width, p.Height)
		if p.Caption != "" {
			body += fmt.Sprintf("<p>%s</p>", html.EscapeString(p.Caption))
		}

		items = append(items, rssItem{
			Title:       title,
			Link:        link,
			GUID:        guid{Value: link, IsPermaLink: true},
			PubDate:     p.Date.Format(time.RFC1123Z),
			Description: body,
			Enclosure:   enclosure{URL: img, Length: p.Bytes, Type: "image/jpeg"},
			MediaThumb:  mediaThumb{URL: a.absURL("media", p.Thumb)},
		})
	}

	feed := rss{
		Version: "2.0",
		AtomNS:  "http://www.w3.org/2005/Atom",
		MediaNS: "http://search.yahoo.com/mrss/",
		ChannelEl: channel{
			Title:       a.cfg.SiteTitle,
			Link:        a.cfg.SiteURL,
			Description: a.cfg.SiteDesc,
			Language:    "en",
			AtomLink:    atomLink{Href: a.absURL("rss.xml"), Rel: "self", Type: "application/rss+xml"},
			Items:       items,
		},
	}
	if len(posts) > 0 {
		feed.ChannelEl.Updated = posts[0].Date.Format(time.RFC1123Z)
	}

	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=600")
	if _, err := w.Write([]byte(xml.Header)); err != nil {
		return
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(feed); err != nil {
		slog.Error("rss encode failed", "err", err)
	}
}
