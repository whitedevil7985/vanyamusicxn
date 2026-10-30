/*
 * ● KanhaMusic
 * ○ A high-performance engine for streaming music in Telegram voicechats.
 *
 * Copyright (C) 2026 Kanha
 *
 * This program is free software: you can redistribute it and/or modify it under the
 * terms of the GNU General Public License as published by the Free Software Foundation,
 * either version 3 of the License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful, but WITHOUT ANY
 * WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS FOR A
 * PARTICULAR PURPOSE. See the GNU General Public License for more details.
 *
 * Repository: https://github.com/Oyekanhaa/KanhaMusic
 */

package platforms

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"KanhaMusic/kanha/logger"

	state "KanhaMusic/kanha/core/models"
	td "github.com/Kanha/Meow"
)

const PlatformRiteshApi state.PlatformName = "RiteshApi"

const defaultRiteshAPIURL = "https://api.riteshyt.in"

const defaultRiteshAPIKey = "riteshfreeb42d698d63bf36be7dd9e5f2"

// maxRiteshBody caps JSON responses so a hostile/proxied reply cannot exhaust memory.
const maxRiteshBody = 8 << 20

// getAPIURL returns the configured API base URL, prioritizing environment variable overrides.
func getAPIURL() string {
	if u := os.Getenv("RITESH_API_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	if u := os.Getenv("ALONE_API_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return defaultRiteshAPIURL
}

// getAPIKey returns the API key from environment variables, falling back to the built-in default.
func getAPIKey() string {
	if k := strings.TrimSpace(os.Getenv("RITESH_API_KEY")); k != "" {
		return k
	}
	if k := strings.TrimSpace(os.Getenv("ALONE_API_KEY")); k != "" {
		return k
	}
	return defaultRiteshAPIKey
}

// riteshHTTP has no client-level timeout: every request is bounded by the
// context passed to Download/Get. Redirects are capped so a redirect loop
// (which the API produces for unknown ids) fails fast instead of spinning.
var riteshHTTP = &http.Client{
	Timeout: 0,
	CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) >= 6 {
			return errors.New("stopped after 6 redirects")
		}
		return nil
	},
}

const riteshUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"

type RiteshApiPlatform struct{}

// riteshDurationSeconds converts "m:ss", "h:mm:ss" or plain seconds into seconds.
func riteshDurationSeconds(d string) int {
	d = strings.TrimSpace(d)
	if d == "" {
		return 0
	}
	total := 0
	for _, part := range strings.Split(d, ":") {
		part = strings.TrimSpace(part)
		n, err := strconv.Atoi(part)
		if err != nil {
			return 0
		}
		total = total*60 + n
	}
	return total
}

// riteshArtwork returns the given thumbnail, or falls back to the YouTube
// thumbnail when the id looks like an 11-char YouTube video id.
func riteshArtwork(thumb, id string) string {
	if thumb = strings.TrimSpace(thumb); thumb != "" {
		return thumb
	}
	if riteshVideoIDRe.MatchString(id) {
		return "https://i.ytimg.com/vi/" + id + "/hqdefault.jpg"
	}
	return ""
}

func init() {
	Register(&RiteshApiPlatform{})
}

func (r *RiteshApiPlatform) Name() state.PlatformName { return PlatformRiteshApi }

// Priority 70: secondary YouTube/Multi-platform downloader. Order: MeowApi (85),
// RiteshApi (70), then yt-dlp (60) as the last fallback.
func (r *RiteshApiPlatform) Priority() int { return 70 }

// CanGet returns true for non-empty search queries or track URLs.
func (r *RiteshApiPlatform) CanGet(query string) bool {
	return strings.TrimSpace(query) != ""
}

var (
	riteshYouTubeURLRe = regexp.MustCompile(`(?i)^https?://([a-z0-9-]+\.)*(youtube\.com|youtu\.be)/`)
	// riteshVideoIDRe is stricter than len(q)==11: a plain text query of 11
	// characters must never be treated as a video id.
	riteshVideoIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	// riteshAppleRe matches Apple Music / iTunes links.
	riteshAppleRe = regexp.MustCompile(`(?i)^https?://(music|itunes)\.apple\.com/`)
	// riteshPreviewRe matches 30-second preview streams (Spotify / Apple).
	riteshPreviewRe = regexp.MustCompile(`(?i)(p\.scdn\.co|mp3-preview|AudioPreview|/api/stream\?url=)`)
	// riteshPlayIDRe extracts the full-length YouTube id from a musicbot play path.
	riteshPlayIDRe = regexp.MustCompile(`/api/musicbot/play/([A-Za-z0-9_-]{11})`)
)

// riteshURLID builds a stable, filename-safe track id for non-YouTube links.
func riteshURLID(link string) string {
	sum := sha1.Sum([]byte(link))
	return "r" + hex.EncodeToString(sum[:])[:14]
}

// riteshPick returns the first non-empty value found for any of the keys,
// looking at the top level and then inside common wrapper objects.
func riteshPick(m map[string]any, keys ...string) string {
	scopes := []map[string]any{m}
	for _, w := range []string{"result", "data", "details", "track", "info"} {
		if sub, ok := m[w].(map[string]any); ok {
			scopes = append(scopes, sub)
		}
	}
	for _, sc := range scopes {
		for _, k := range keys {
			switch v := sc[k].(type) {
			case string:
				if strings.TrimSpace(v) != "" {
					return strings.TrimSpace(v)
				}
			case float64:
				if v > 0 {
					return strconv.FormatInt(int64(v), 10)
				}
			}
		}
	}
	return ""
}

// riteshBodySnippet returns a short, single-line preview of a response body for logs.
func riteshBodySnippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// riteshAbs resolves a possibly-relative API path against the API base.
// /api/platform/extract returns URLs like "/api/musicbot/play/<id>", and
// feeding those to http.Get fails with "unsupported protocol scheme".
func riteshAbs(u string) string {
	u = strings.TrimSpace(u)
	if u == "" || strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	return strings.TrimRight(getAPIURL(), "/") + "/" + strings.TrimLeft(u, "/")
}

// riteshGetJSON performs a GET and decodes the JSON object. 5xx answers are
// retried once: Cloudflare returns sporadic 502s that succeed on the next try.
func riteshGetJSON(ctx context.Context, path string, params url.Values) (map[string]any, []byte, error) {
	apiKey := getAPIKey()
	if apiKey != "" && params.Get("api_key") == "" {
		params.Set("api_key", apiKey)
	}
	endpoint := getAPIURL() + path
	if enc := params.Encode(); enc != "" {
		endpoint += "?" + enc
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("User-Agent", riteshUserAgent)
		req.Header.Set("Accept", "application/json")

		resp, err := riteshHTTP.Do(req)
		if err != nil {
			return nil, nil, sanitizeAPIError(fmt.Errorf("riteshapi %s failed: %w", path, err), apiKey)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxRiteshBody))
		resp.Body.Close()
		if readErr != nil {
			return nil, nil, fmt.Errorf("riteshapi %s read failed: %w", path, readErr)
		}

		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return nil, body, fmt.Errorf("riteshapi %s unauthorized (http %d) — check RITESH_API_KEY", path, resp.StatusCode)
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("riteshapi %s http %d: %s", path, resp.StatusCode, riteshBodySnippet(body))
			time.Sleep(400 * time.Millisecond)
			continue
		case resp.StatusCode < 200 || resp.StatusCode >= 300:
			return nil, body, fmt.Errorf("riteshapi %s http %d: %s", path, resp.StatusCode, riteshBodySnippet(body))
		}

		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, body, fmt.Errorf("riteshapi %s bad JSON: %s", path, riteshBodySnippet(body))
		}
		return raw, body, nil
	}
	return nil, nil, lastErr
}

// ── /api/platform/extract ───────────────────────────────────────────────────

type riteshFormat struct {
	Type      string  `json:"type"`
	URL       string  `json:"url"`
	DirectURL string  `json:"direct_url"`
	Ext       string  `json:"ext"`
	Bitrate   float64 `json:"bitrate"`
	HasAudio  bool    `json:"has_audio"`
}

type riteshExtractResp struct {
	Success     bool           `json:"success"`
	Platform    string         `json:"platform"`
	ID          string         `json:"id"`
	Title       string         `json:"title"`
	Thumbnail   string         `json:"thumbnail"`
	Duration    float64        `json:"duration"`
	Formats     []riteshFormat `json:"formats"`
	DownloadURL string         `json:"download_url"`
	Author      struct {
		Name string `json:"name"`
	} `json:"author"`
}

// riteshExtract calls /api/platform/extract, the multi-platform endpoint
// (YouTube, Spotify, Apple Music, SoundCloud, Instagram, TikTok, ...).
func riteshExtract(ctx context.Context, link string) (*riteshExtractResp, error) {
	raw, body, err := riteshGetJSON(ctx, "/api/platform/extract", url.Values{"url": {link}})
	if err != nil {
		return nil, err
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var out riteshExtractResp
	if err := json.Unmarshal(blob, &out); err != nil {
		return nil, fmt.Errorf("riteshapi extract bad shape: %s", riteshBodySnippet(body))
	}
	if !out.Success || strings.TrimSpace(out.Title) == "" {
		return nil, fmt.Errorf("riteshapi extract unsuccessful: %s", riteshBodySnippet(body))
	}
	return &out, nil
}

// riteshPickMedia chooses the playable URL for the requested media type.
// Full-length "/api/musicbot/play/<id>" streams always win over the 30-second
// preview URLs that Spotify/Apple formats also expose.
func riteshPickMedia(x *riteshExtractResp, video bool) (string, string) {
	if x == nil {
		return "", ""
	}
	bestPlay, bestPlayExt := "", ""
	bestOther, bestOtherExt := "", ""
	bestVideo, bestVideoExt := "", ""

	for _, f := range x.Formats {
		u := strings.TrimSpace(f.URL)
		if u == "" {
			u = strings.TrimSpace(f.DirectURL)
		}
		if u == "" {
			continue
		}
		switch {
		case riteshPlayIDRe.MatchString(u):
			if bestPlay == "" {
				bestPlay, bestPlayExt = u, f.Ext
			}
		case video && strings.EqualFold(f.Type, "video") && f.HasAudio:
			if bestVideo == "" {
				bestVideo, bestVideoExt = u, f.Ext
			}
		case !video && strings.EqualFold(f.Type, "audio") && !riteshPreviewRe.MatchString(u):
			if bestOther == "" {
				bestOther, bestOtherExt = u, f.Ext
			}
		}
	}

	if video {
		if bestVideo != "" {
			return riteshAbs(bestVideo), bestVideoExt
		}
		return riteshAbs(x.DownloadURL), ""
	}
	if bestPlay != "" {
		return riteshAbs(bestPlay), bestPlayExt
	}
	if bestOther != "" {
		return riteshAbs(bestOther), bestOtherExt
	}
	return riteshAbs(x.DownloadURL), ""
}

// riteshURLFallback resolves a non-YouTube link when the extractor gave
// nothing usable: first via yt-dlp metadata, otherwise a minimal track so the
// download chain still gets a chance.
func riteshURLFallback(link string, isVideo bool) []*state.Track {
	if yt, ok := GetPlatform(PlatformYtDlp); ok {
		if got, err := yt.Get(link, isVideo); err == nil && len(got) > 0 {
			return got
		}
	}
	title := link
	if u, err := url.Parse(link); err == nil && u.Host != "" {
		title = u.Host
	}
	return []*state.Track{{
		ID:     riteshURLID(link),
		Title:  title,
		URL:    link,
		Source: PlatformYtDlp,
		Video:  isVideo,
	}}
}

// riteshTrackFromExtract turns an extract response into a Track. YouTube ids
// keep the PlatformYouTube source so the rest of the pipeline treats them like
// any other YouTube result.
func riteshTrackFromExtract(link string, x *riteshExtractResp, isVideo bool) *state.Track {
	ytID := ""
	if x.Platform == "youtube" && riteshVideoIDRe.MatchString(x.ID) {
		ytID = x.ID
	}
	if u, _ := riteshPickMedia(x, isVideo); u != "" {
		if m := riteshPlayIDRe.FindStringSubmatch(u); m != nil {
			ytID = m[1]
		}
	}
	t := &state.Track{
		Title:    strings.TrimSpace(x.Title),
		URL:      link,
		Duration: int(x.Duration + 0.5),
		Artwork:  riteshArtwork(x.Thumbnail, ytID),
		Video:    isVideo,
		Source:   PlatformYtDlp,
		ID:       riteshURLID(link),
	}
	if ytID != "" {
		t.ID = ytID
		t.Source = PlatformYouTube
		t.Artwork = riteshArtwork(x.Thumbnail, ytID)
	}
	return t
}

// ── /details (bare video ids only) ──────────────────────────────────────────

// riteshDetails looks up metadata for a bare 11-char id.
//
// It is deliberately NOT used for full URLs: /details?link=<url> currently
// answers with a completely different video (title = the input URL, vidid =
// an unrelated id), which used to poison the queue with the wrong song.
func riteshDetails(ctx context.Context, videoID string) (*state.Track, error) {
	raw, body, err := riteshGetJSON(ctx, "/details", url.Values{"link": {videoID}})
	if err != nil {
		return nil, err
	}
	vid := riteshPick(raw, "vidid", "video_id")
	title := riteshPick(raw, "title", "name")
	if vid != videoID || title == "" || title == videoID || strings.HasPrefix(title, "http") {
		return nil, fmt.Errorf("riteshapi /details returned junk: %s", riteshBodySnippet(body))
	}
	dur := 0
	if n, ok := raw["duration_sec"].(float64); ok && n > 0 {
		dur = int(n)
	} else {
		dur = riteshDurationSeconds(riteshPick(raw, "duration_min", "duration"))
	}
	return &state.Track{
		ID:       videoID,
		Title:    title,
		URL:      riteshPick(raw, "link", "url", "webpage_url"),
		Duration: dur,
		Artwork:  riteshArtwork(riteshPick(raw, "thumbnail", "thumb", "cover"), videoID),
		Source:   PlatformYouTube,
	}, nil
}

// ── /search ─────────────────────────────────────────────────────────────────

type riteshSearchResp struct {
	Result []struct {
		ID         string `json:"id"`
		Title      string `json:"title"`
		Link       string `json:"link"`
		Duration   string `json:"duration"`
		Thumbnails []struct {
			URL string `json:"url"`
		} `json:"thumbnails"`
		Channel struct {
			Name string `json:"name"`
		} `json:"channel"`
	} `json:"result"`
}

func riteshSearch(ctx context.Context, query string, isVideo bool) ([]*state.Track, error) {
	raw, body, err := riteshGetJSON(ctx, "/search", url.Values{
		"q":     {query},
		"limit": {"5"},
	})
	if err != nil {
		return nil, err
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var parsed riteshSearchResp
	if err := json.Unmarshal(blob, &parsed); err != nil {
		return nil, fmt.Errorf("riteshapi search bad shape: %s", riteshBodySnippet(body))
	}
	if len(parsed.Result) == 0 {
		return nil, errors.New("riteshapi: no search results found")
	}

	tracks := make([]*state.Track, 0, len(parsed.Result))
	for _, item := range parsed.Result {
		if !riteshVideoIDRe.MatchString(item.ID) {
			continue
		}
		thumb := ""
		if n := len(item.Thumbnails); n > 0 {
			thumb = item.Thumbnails[n-1].URL
		}
		link := item.Link
		if link == "" {
			link = "https://www.youtube.com/watch?v=" + item.ID
		}
		tracks = append(tracks, &state.Track{
			ID:       item.ID,
			Title:    item.Title,
			URL:      link,
			Duration: riteshDurationSeconds(item.Duration),
			Artwork:  riteshArtwork(thumb, item.ID),
			Source:   PlatformYouTube,
			Video:    isVideo,
		})
	}
	if len(tracks) == 0 {
		return nil, errors.New("riteshapi: no usable search results")
	}
	// A text search must queue only the top match (same as the YouTube platform),
	// otherwise the other results get added to the queue as extra tracks.
	return tracks[:1], nil
}

// Get performs search or details retrieval via RiteshApi endpoints.
func (r *RiteshApiPlatform) Get(query string, isVideo bool) ([]*state.Track, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, errors.New("riteshapi: empty query")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Link: the extractor is the only endpoint that returns correct metadata
	// for a URL (see the /details note above).
	if strings.HasPrefix(q, "http://") || strings.HasPrefix(q, "https://") {
		if x, err := riteshExtract(ctx, q); err == nil {
			if tr := riteshTrackFromExtract(q, x, isVideo); tr != nil && tr.Title != "" {
				return []*state.Track{tr}, nil
			}
		} else {
			logger.Warnf("RiteshApi extract failed: %v", err)
		}
		return riteshURLFallback(q, isVideo), nil
	}

	// Bare video id.
	if riteshVideoIDRe.MatchString(q) {
		if tr, err := riteshDetails(ctx, q); err == nil && tr != nil {
			tr.Video = isVideo
			return []*state.Track{tr}, nil
		} else if err != nil {
			logger.Warnf("RiteshApi /details failed: %v", err)
		}
	}

	return riteshSearch(ctx, q, isVideo)
}

// CanDownload checks if RiteshApi can handle downloading for the given platform source.
// RiteshApi is a multi-platform engine supporting YouTube, Spotify, JioSaavn,
// Apple Music, SoundCloud, Instagram, TikTok, Twitter/X, Facebook, Deezer, and 20+ other platforms.
func (r *RiteshApiPlatform) CanDownload(source state.PlatformName) bool {
	if getAPIURL() == "" {
		return false
	}
	switch source {
	case PlatformYouTube,
		PlatformSpotify,
		state.PlatformName("JioSaavn"),
		state.PlatformName("AppleMusic"),
		state.PlatformName("Resso"),
		state.PlatformName("SoundCloud"),
		state.PlatformName("Instagram"),
		state.PlatformName("TikTok"),
		state.PlatformName("Twitter"),
		state.PlatformName("Facebook"),
		PlatformDeezer,
		state.PlatformName("Bilibili"),
		state.PlatformName("Pinterest"),
		state.PlatformName("Gaana"),
		state.PlatformName("Tidal"),
		state.PlatformName("Dailymotion"),
		state.PlatformName("Reddit"),
		state.PlatformName("Snapchat"),
		state.PlatformName("Twitch"),
		state.PlatformName("Kick"),
		state.PlatformName("Terabox"),
		state.PlatformName("Threads"),
		state.PlatformName("Tumblr"),
		state.PlatformName("MXPlayer"),
		state.PlatformName("IMDb"),
		state.PlatformName("Bluesky"),
		state.PlatformName("Mastodon"),
		state.PlatformName("LinkedIn"):
		return true
	default:
		// Accept any non-empty source as RiteshApi acts as a universal multi-platform engine.
		return source != ""
	}
}

// ── download ────────────────────────────────────────────────────────────────

// riteshLooksLikeMedia validates the container by magic bytes. The API serves
// MP4/M4A payloads as "audio/mpeg" and answers errors with JSON, so the
// extension and the Content-Type header are both useless here.
func riteshLooksLikeMedia(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 16)
	n, _ := io.ReadFull(f, head)
	if n < 4 {
		return false
	}
	head = head[:n]
	switch {
	case head[0] == '{' || head[0] == '<':
		return false
	case string(head[4:min(8, n)]) == "ftyp":
		return true
	case string(head[:min(3, n)]) == "ID3":
		return true
	case string(head[:min(4, n)]) == "OggS",
		string(head[:min(4, n)]) == "RIFF",
		string(head[:min(4, n)]) == "fLaC":
		return true
	case n >= 2 && head[0] == 0xFF && (head[1] == 0xFB || head[1] == 0xF3 || head[1] == 0xF2):
		return true
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// riteshFetchTo streams url into path via a .part file so a half-written
// download can never be mistaken for a cached track.
func riteshFetchTo(ctx context.Context, rawURL, path string, timeout time.Duration) (retry bool, err error) {
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(dctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", riteshUserAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := riteshHTTP.Do(req)
	if err != nil {
		if dctx.Err() != nil {
			return false, dctx.Err()
		}
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return true, fmt.Errorf("http %d", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Errorf("http %d: %s", resp.StatusCode, riteshBodySnippet(snippet))
	}

	// The cache directory may not exist yet on a fresh install.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, err
		}
	}

	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return false, err
	}
	written, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(tmp)
		if dctx.Err() != nil {
			return false, dctx.Err()
		}
		return true, fmt.Errorf("body copy failed: %w", copyErr)
	}
	if written < int64(minValidDownloadBytes) || !riteshLooksLikeMedia(tmp) {
		os.Remove(tmp)
		return false, fmt.Errorf("empty or non-media payload (%d bytes)", written)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return false, err
	}
	return false, nil
}

func (r *RiteshApiPlatform) Download(
	ctx context.Context,
	track *state.Track,
	_ *td.Message,
) (string, error) {
	if p := findFile(track); p != "" {
		logger.Debug("RiteshApi: cache hit " + p)
		return p, nil
	}

	// Resolve the query the API understands: a bare YouTube id for YouTube
	// tracks, the real link for extractor tracks, then id/url/title fallbacks.
	query := ""
	if track.Source != PlatformYouTube && strings.HasPrefix(track.URL, "http") {
		query = strings.TrimSpace(track.URL)
	}
	if query == "" {
		query = strings.TrimSpace(track.ID)
	}
	if query == "" {
		query = strings.TrimSpace(track.URL)
	}
	if query == "" {
		query = strings.TrimSpace(track.Title)
	}
	if query == "" {
		return "", errors.New("riteshapi: missing video id or track query")
	}

	dtype, ext, timeout := "audio", ".mp3", audioDownloadTimeout
	if track.Video {
		dtype, ext, timeout = "video", ".mp4", videoDownloadTimeout
	}

	path := getPath(track, ext)
	apiKey := getAPIKey()
	apiURL := getAPIURL()

	type attempt struct {
		label string
		url   string
	}
	attempts := []attempt{
		{"/download", fmt.Sprintf("%s/download?%s", apiURL, url.Values{
			"query": {query}, "dl_type": {dtype}, "api_key": {apiKey},
		}.Encode())},
		{"/downloads/stream", fmt.Sprintf("%s/downloads/stream?%s", apiURL, url.Values{
			"query": {query}, "dl_type": {dtype}, "api_key": {apiKey},
		}.Encode())},
	}

	// URLs can also be served through the extractor, which is the only working
	// path for a few platforms.
	if strings.HasPrefix(query, "http://") || strings.HasPrefix(query, "https://") {
		ectx, ecancel := context.WithTimeout(ctx, 30*time.Second)
		if x, err := riteshExtract(ectx, query); err == nil {
			if media, _ := riteshPickMedia(x, track.Video); media != "" {
				attempts = append(attempts, attempt{"extract", media})
			}
		} else {
			logger.Warnf("RiteshApi extract for download failed: %v", err)
		}
		ecancel()
	}

	var lastErr error
	for _, a := range attempts {
		for try := 0; try < 2; try++ {
			retry, err := riteshFetchTo(ctx, a.url, path, timeout)
			if err == nil {
				logger.Debug("RiteshApi: downloaded via " + a.label + ": " + path)
				return path, nil
			}
			if isDownloadCancelled(err) {
				os.Remove(path)
				return "", err
			}
			lastErr = sanitizeAPIError(err, apiKey)
			logger.Warnf("RiteshApi %s failed: %v", a.label, lastErr)
			if retry && try == 0 {
				time.Sleep(400 * time.Millisecond)
				continue
			}
			break
		}
	}

	os.Remove(path)
	if lastErr == nil {
		lastErr = errors.New("no endpoint available")
	}
	return "", fmt.Errorf("riteshapi download failed: %w", lastErr)
}
