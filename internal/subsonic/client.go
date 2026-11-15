// Package subsonic is a small client for the Subsonic REST API (as
// implemented by Navidrome and compatible servers).
package subsonic

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// APIVersion is the Subsonic API version sent with every request.
	APIVersion = "1.16.1"
	// ClientName identifies Navigo to the server.
	ClientName = "navigo"
)

// Error is a Subsonic envelope error (status="failed").
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	switch e.Code {
	case 40:
		return "wrong username or password"
	case 41:
		return "token authentication not supported by server"
	case 50:
		return "user is not authorized for this operation"
	case 70:
		if e.Message != "" {
			return "not found: " + e.Message
		}
		return "requested item not found"
	}
	if e.Message != "" {
		return fmt.Sprintf("server error %d: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("server error %d", e.Code)
}

// HTTPError is returned for non-2xx HTTP responses.
type HTTPError struct {
	StatusCode int
	Status     string
}

func (e *HTTPError) Error() string { return "HTTP error: " + e.Status }

// ErrTimeout is returned (wrapped) when a request times out.
var ErrTimeout = errors.New("request timed out")

// ReplayGain holds ReplayGain values reported by the server.
type ReplayGain struct {
	TrackGain float64 `json:"trackGain"`
	AlbumGain float64 `json:"albumGain"`
	TrackPeak float64 `json:"trackPeak"`
	AlbumPeak float64 `json:"albumPeak"`
}

// Song is a track ("child" in Subsonic terms).
type Song struct {
	ID          string      `json:"id"`
	Title       string      `json:"title"`
	Album       string      `json:"album,omitempty"`
	AlbumID     string      `json:"albumId,omitempty"`
	Artist      string      `json:"artist,omitempty"`
	ArtistID    string      `json:"artistId,omitempty"`
	AlbumArtist string      `json:"displayAlbumArtist,omitempty"`
	CoverArt    string      `json:"coverArt,omitempty"`
	Suffix      string      `json:"suffix,omitempty"`
	ContentType string      `json:"contentType,omitempty"`
	Duration    int         `json:"duration,omitempty"`
	Track       int         `json:"track,omitempty"`
	DiscNumber  int         `json:"discNumber,omitempty"`
	Year        int         `json:"year,omitempty"`
	BitRate     int         `json:"bitRate,omitempty"`
	Size        int64       `json:"size,omitempty"`
	ReplayGain  *ReplayGain `json:"replayGain,omitempty"`
}

// DurationTime returns the song duration as a time.Duration.
func (s Song) DurationTime() time.Duration { return time.Duration(s.Duration) * time.Second }

// Album is an ID3 album.
type Album struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Artist    string `json:"artist"`
	ArtistID  string `json:"artistId"`
	CoverArt  string `json:"coverArt"`
	SongCount int    `json:"songCount"`
	Duration  int    `json:"duration"`
	Year      int    `json:"year"`
	Song      []Song `json:"song"`
}

// Artist is an ID3 artist.
type Artist struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	CoverArt   string  `json:"coverArt"`
	AlbumCount int     `json:"albumCount"`
	Album      []Album `json:"album"`
}

// Playlist is a server playlist.
type Playlist struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Owner     string `json:"owner"`
	CoverArt  string `json:"coverArt"`
	SongCount int    `json:"songCount"`
	Duration  int    `json:"duration"`
	Entry     []Song `json:"entry"`
}

// SearchResult is the result of search3.
type SearchResult struct {
	Artists []Artist `json:"artist"`
	Albums  []Album  `json:"album"`
	Songs   []Song   `json:"song"`
}

// Response is the decoded "subsonic-response" envelope.
type Response struct {
	Status     string `json:"status"`
	Version    string `json:"version"`
	Error      *Error `json:"error"`
	AlbumList2 *struct {
		Album []Album `json:"album"`
	} `json:"albumList2"`
	Album   *Album  `json:"album"`
	Artist  *Artist `json:"artist"`
	Artists *struct {
		Index []struct {
			Name   string   `json:"name"`
			Artist []Artist `json:"artist"`
		} `json:"index"`
	} `json:"artists"`
	Playlists *struct {
		Playlist []Playlist `json:"playlist"`
	} `json:"playlists"`
	Playlist      *Playlist     `json:"playlist"`
	SearchResult3 *SearchResult `json:"searchResult3"`
	Song          *Song         `json:"song"`
}

type envelope struct {
	R Response `json:"subsonic-response"`
}

// ParseResponse decodes a JSON envelope and returns an error for failed
// responses.
func ParseResponse(r io.Reader) (*Response, error) {
	var env envelope
	if err := json.NewDecoder(r).Decode(&env); err != nil {
		return nil, fmt.Errorf("invalid server response: %w", err)
	}
	if env.R.Status == "" {
		return nil, errors.New("invalid server response: not a Subsonic API endpoint")
	}
	if env.R.Status != "ok" {
		if env.R.Error != nil {
			return nil, env.R.Error
		}
		return nil, errors.New("server returned status " + env.R.Status)
	}
	return &env.R, nil
}

// Token computes the Subsonic auth token md5(password+salt) as lowercase hex.
func Token(password, salt string) string {
	sum := md5.Sum([]byte(password + salt))
	return hex.EncodeToString(sum[:])
}

// NewSalt returns a fresh random salt.
func NewSalt() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// Client talks to one server.
type Client struct {
	base     *url.URL
	user     string
	password string
	// api is used for small JSON requests (short timeout).
	api *http.Client
	// stream is used for media downloads (no overall timeout).
	stream *http.Client
}

// New creates a client. baseURL may include a path prefix.
func New(baseURL, user, password string, insecureTLS bool) (*Client, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return nil, errors.New("server URL is empty")
	}
	if !strings.Contains(baseURL, "://") {
		baseURL = "https://" + baseURL
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid server URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("invalid server URL scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("invalid server URL: missing host")
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   6,
		ForceAttemptHTTP2:     true,
	}
	if insecureTLS {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // user opt-in
	}
	return &Client{
		base:     u,
		user:     user,
		password: password,
		api:      &http.Client{Transport: tr, Timeout: 20 * time.Second},
		stream:   &http.Client{Transport: tr},
	}, nil
}

// BaseURL returns the normalized server URL.
func (c *Client) BaseURL() string { return c.base.String() }

// Username returns the configured user.
func (c *Client) Username() string { return c.user }

func (c *Client) authParams() url.Values {
	salt := NewSalt()
	v := url.Values{}
	v.Set("u", c.user)
	v.Set("t", Token(c.password, salt))
	v.Set("s", salt)
	v.Set("v", APIVersion)
	v.Set("c", ClientName)
	v.Set("f", "json")
	return v
}

// URL builds an authenticated endpoint URL.
func (c *Client) URL(endpoint string, params url.Values) string {
	u := *c.base
	u.Path = c.base.Path + "/rest/" + endpoint
	q := c.authParams()
	for k, vs := range params {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func wrapNetErr(err error) error {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return fmt.Errorf("%w: %v", ErrTimeout, err)
	}
	return err
}

func (c *Client) call(ctx context.Context, endpoint string, params url.Values) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL(endpoint, params), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.api.Do(req)
	if err != nil {
		return nil, wrapNetErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, &HTTPError{StatusCode: resp.StatusCode, Status: resp.Status}
	}
	r, err := ParseResponse(resp.Body)
	if err != nil {
		return nil, wrapNetErr(err)
	}
	return r, nil
}

// Ping checks connectivity and credentials.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.call(ctx, "ping", nil)
	return err
}

// Album list types for GetAlbumList2.
const (
	ListNewest       = "newest"
	ListRecent       = "recent"
	ListFrequent     = "frequent"
	ListRandom       = "random"
	ListAlphabetical = "alphabeticalByName"
)

// GetAlbumList2 returns a page of albums.
func (c *Client) GetAlbumList2(ctx context.Context, typ string, size, offset int) ([]Album, error) {
	v := url.Values{}
	v.Set("type", typ)
	v.Set("size", strconv.Itoa(size))
	v.Set("offset", strconv.Itoa(offset))
	r, err := c.call(ctx, "getAlbumList2", v)
	if err != nil {
		return nil, err
	}
	if r.AlbumList2 == nil {
		return nil, nil
	}
	return r.AlbumList2.Album, nil
}

// GetAlbum returns an album including its songs.
func (c *Client) GetAlbum(ctx context.Context, id string) (*Album, error) {
	r, err := c.call(ctx, "getAlbum", url.Values{"id": {id}})
	if err != nil {
		return nil, err
	}
	if r.Album == nil {
		return nil, &Error{Code: 70, Message: "album"}
	}
	return r.Album, nil
}

// GetArtists returns all artists (flattened from the index).
func (c *Client) GetArtists(ctx context.Context) ([]Artist, error) {
	r, err := c.call(ctx, "getArtists", nil)
	if err != nil {
		return nil, err
	}
	var out []Artist
	if r.Artists != nil {
		for _, idx := range r.Artists.Index {
			out = append(out, idx.Artist...)
		}
	}
	return out, nil
}

// GetArtist returns an artist with albums.
func (c *Client) GetArtist(ctx context.Context, id string) (*Artist, error) {
	r, err := c.call(ctx, "getArtist", url.Values{"id": {id}})
	if err != nil {
		return nil, err
	}
	if r.Artist == nil {
		return nil, &Error{Code: 70, Message: "artist"}
	}
	return r.Artist, nil
}

// GetPlaylists lists playlists.
func (c *Client) GetPlaylists(ctx context.Context) ([]Playlist, error) {
	r, err := c.call(ctx, "getPlaylists", nil)
	if err != nil {
		return nil, err
	}
	if r.Playlists == nil {
		return nil, nil
	}
	return r.Playlists.Playlist, nil
}

// GetPlaylist returns a playlist with entries.
func (c *Client) GetPlaylist(ctx context.Context, id string) (*Playlist, error) {
	r, err := c.call(ctx, "getPlaylist", url.Values{"id": {id}})
	if err != nil {
		return nil, err
	}
	if r.Playlist == nil {
		return nil, &Error{Code: 70, Message: "playlist"}
	}
	return r.Playlist, nil
}

// GetSong returns one song.
func (c *Client) GetSong(ctx context.Context, id string) (*Song, error) {
	r, err := c.call(ctx, "getSong", url.Values{"id": {id}})
	if err != nil {
		return nil, err
	}
	if r.Song == nil {
		return nil, &Error{Code: 70, Message: "song"}
	}
	return r.Song, nil
}

// Search3 searches artists, albums and songs.
func (c *Client) Search3(ctx context.Context, query string, artistCount, albumCount, songCount int) (*SearchResult, error) {
	v := url.Values{}
	v.Set("query", query)
	v.Set("artistCount", strconv.Itoa(artistCount))
	v.Set("albumCount", strconv.Itoa(albumCount))
	v.Set("songCount", strconv.Itoa(songCount))
	r, err := c.call(ctx, "search3", v)
	if err != nil {
		return nil, err
	}
	if r.SearchResult3 == nil {
		return &SearchResult{}, nil
	}
	return r.SearchResult3, nil
}

// Scrobble registers playback. submission=false sends "now playing".
func (c *Client) Scrobble(ctx context.Context, id string, submission bool, at time.Time) error {
	v := url.Values{}
	v.Set("id", id)
	v.Set("submission", strconv.FormatBool(submission))
	if submission {
		v.Set("time", strconv.FormatInt(at.UnixMilli(), 10))
	}
	_, err := c.call(ctx, "scrobble", v)
	return err
}

// CoverArtURL returns the URL of a cover image.
func (c *Client) CoverArtURL(id string, size int) string {
	v := url.Values{"id": {id}}
	if size > 0 {
		v.Set("size", strconv.Itoa(size))
	}
	return c.URL("getCoverArt", v)
}

// GetCoverArt downloads a cover image.
func (c *Client) GetCoverArt(ctx context.Context, id string, size int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.CoverArtURL(id, size), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.api.Do(req)
	if err != nil {
		return nil, wrapNetErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{StatusCode: resp.StatusCode, Status: resp.Status}
	}
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "json") || strings.Contains(ct, "xml") {
		if _, err := ParseResponse(resp.Body); err != nil {
			return nil, err
		}
		return nil, errors.New("server returned no image")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return b, wrapNetErr(err)
}

// StreamURL returns the stream URL. format "" or "raw" means original.
func (c *Client) StreamURL(id, format string, maxBitRate int) string {
	v := url.Values{"id": {id}}
	if format != "" {
		v.Set("format", format)
	}
	if maxBitRate > 0 {
		v.Set("maxBitRate", strconv.Itoa(maxBitRate))
	}
	return c.URL("stream", v)
}

// Stream opens a media stream starting at byte offset (Range request when
// offset > 0). The caller must close the response body. A server that ignores
// the Range header answers 200; callers should check resp.StatusCode.
func (c *Client) Stream(ctx context.Context, id, format string, maxBitRate int, offset int64) (*http.Response, error) {
	return c.media(ctx, c.StreamURL(id, format, maxBitRate), offset)
}

// Download opens the original file (download endpoint).
func (c *Client) Download(ctx context.Context, id string, offset int64) (*http.Response, error) {
	return c.media(ctx, c.URL("download", url.Values{"id": {id}}), offset)
}

func (c *Client) media(ctx context.Context, u string, offset int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, wrapNetErr(err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, &HTTPError{StatusCode: resp.StatusCode, Status: resp.Status}
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/json") || strings.HasPrefix(ct, "text/xml") || strings.HasPrefix(ct, "application/xml") {
		defer resp.Body.Close()
		if _, err := ParseResponse(resp.Body); err != nil {
			return nil, err
		}
		return nil, errors.New("server returned no media")
	}
	return resp, nil
}
