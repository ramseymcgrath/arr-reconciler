package arr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ramseymcgrath/arr-reconciler/internal/config"
)

// StatusError is returned by do for a non-2xx HTTP response, exposing the code
// so callers can react to specific statuses (e.g. treat 404 on a delete as
// already-done rather than a failure).
type StatusError struct {
	Method string
	Path   string
	Code   int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: status %d: %s", e.Method, e.Path, e.Code, e.Body)
}

// Client talks to a single Sonarr or Radarr v3 instance.
type Client struct {
	name    string
	kind    string
	baseURL string
	apiKey  string
	http    *http.Client
}

// New constructs a Client for the given instance.
func New(inst config.Instance, timeout time.Duration) *Client {
	return &Client{
		name:    inst.Name,
		kind:    inst.Kind,
		baseURL: inst.BaseURL,
		apiKey:  inst.APIKey,
		http:    &http.Client{Timeout: timeout},
	}
}

func (c *Client) Name() string { return c.name }
func (c *Client) Kind() string { return c.kind }

func (c *Client) do(ctx context.Context, method, path string, query url.Values, out any) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return fmt.Errorf("build request %s %s: %w", method, path, err)
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("read response %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &StatusError{Method: method, Path: path, Code: resp.StatusCode, Body: string(body)}
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response %s %s: %w", method, path, err)
	}
	return nil
}

// QueueRecord is a single item in the download queue. Field set is the union
// of Sonarr and Radarr v3 shapes; instance-specific fields stay nil/zero.
type QueueRecord struct {
	ID                    int64      `json:"id"`
	Title                 string     `json:"title"`
	Status                string     `json:"status"`
	TrackedDownloadStatus string     `json:"trackedDownloadStatus"`
	TrackedDownloadState  string     `json:"trackedDownloadState"`
	ErrorMessage          string     `json:"errorMessage"`
	Size                  float64    `json:"size"`
	Sizeleft              float64    `json:"sizeleft"`
	Added                 time.Time  `json:"added"`
	EstimatedCompletion   *time.Time `json:"estimatedCompletionTime"`
	DownloadID            string     `json:"downloadId"`
	Protocol              string     `json:"protocol"`
	Indexer               string     `json:"indexer"`
	StatusMessages        []struct {
		Title    string   `json:"title"`
		Messages []string `json:"messages"`
	} `json:"statusMessages"`
}

type queuePage struct {
	Page         int           `json:"page"`
	PageSize     int           `json:"pageSize"`
	TotalRecords int           `json:"totalRecords"`
	Records      []QueueRecord `json:"records"`
}

// Queue fetches the entire download queue, paging through all records.
func (c *Client) Queue(ctx context.Context) ([]QueueRecord, error) {
	const pageSize = 100
	var all []QueueRecord
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("pageSize", strconv.Itoa(pageSize))
		q.Set("includeUnknownSeriesItems", "true")
		q.Set("includeUnknownMovieItems", "true")
		var pg queuePage
		if err := c.do(ctx, http.MethodGet, "/api/v3/queue", q, &pg); err != nil {
			return nil, fmt.Errorf("%s: queue page %d: %w", c.name, page, err)
		}
		all = append(all, pg.Records...)
		if len(all) >= pg.TotalRecords || len(pg.Records) == 0 {
			break
		}
	}
	return all, nil
}

// DeleteQueueItem removes an item from the queue, optionally removing it from
// the download client and blocklisting the release so it is not re-grabbed.
//
// A 404 is treated as success: season-pack downloads produce several queue
// records sharing one downloadId, so deleting the first (with removeFromClient)
// drops the whole download and the sibling records vanish. A later delete of a
// now-gone sibling 404s, but the desired end state — the item is gone — already
// holds, so it is not an error.
func (c *Client) DeleteQueueItem(ctx context.Context, id int64, removeFromClient, blocklist bool) error {
	q := url.Values{}
	q.Set("removeFromClient", strconv.FormatBool(removeFromClient))
	q.Set("blocklist", strconv.FormatBool(blocklist))
	q.Set("skipRedownload", "false")
	path := "/api/v3/queue/" + strconv.FormatInt(id, 10)
	err := c.do(ctx, http.MethodDelete, path, q, nil)
	var se *StatusError
	if errors.As(err, &se) && se.Code == http.StatusNotFound {
		return nil
	}
	return err
}

// MediaFile is a file the arr instance believes exists on disk.
type MediaFile struct {
	ID   int64  `json:"id"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// TrackedFiles returns every file path the instance has recorded, across all
// series (Sonarr) or movies (Radarr). These are the files that SHOULD exist.
func (c *Client) TrackedFiles(ctx context.Context) ([]MediaFile, error) {
	switch c.kind {
	case "sonarr":
		return c.sonarrEpisodeFiles(ctx)
	case "radarr":
		return c.radarrMovieFiles(ctx)
	default:
		return nil, fmt.Errorf("%s: unknown kind %q", c.name, c.kind)
	}
}

func (c *Client) sonarrEpisodeFiles(ctx context.Context) ([]MediaFile, error) {
	var series []struct {
		ID int64 `json:"id"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v3/series", nil, &series); err != nil {
		return nil, fmt.Errorf("%s: list series: %w", c.name, err)
	}
	var files []MediaFile
	for _, s := range series {
		q := url.Values{}
		q.Set("seriesId", strconv.FormatInt(s.ID, 10))
		var efs []MediaFile
		if err := c.do(ctx, http.MethodGet, "/api/v3/episodefile", q, &efs); err != nil {
			return nil, fmt.Errorf("%s: episodefile series %d: %w", c.name, s.ID, err)
		}
		files = append(files, efs...)
	}
	return files, nil
}

func (c *Client) radarrMovieFiles(ctx context.Context) ([]MediaFile, error) {
	var movies []struct {
		ID        int64 `json:"id"`
		HasFile   bool  `json:"hasFile"`
		MovieFile *struct {
			ID   int64  `json:"id"`
			Path string `json:"path"`
			Size int64  `json:"size"`
		} `json:"movieFile"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v3/movie", nil, &movies); err != nil {
		return nil, fmt.Errorf("%s: list movies: %w", c.name, err)
	}
	var files []MediaFile
	for _, m := range movies {
		if m.HasFile && m.MovieFile != nil && m.MovieFile.Path != "" {
			files = append(files, MediaFile{
				ID:   m.MovieFile.ID,
				Path: m.MovieFile.Path,
				Size: m.MovieFile.Size,
			})
		}
	}
	return files, nil
}

// RootFolder is a configured library root path on the instance.
type RootFolder struct {
	ID   int64  `json:"id"`
	Path string `json:"path"`
}

// RootFolders returns the configured library roots for this instance.
func (c *Client) RootFolders(ctx context.Context) ([]RootFolder, error) {
	var rfs []RootFolder
	if err := c.do(ctx, http.MethodGet, "/api/v3/rootfolder", nil, &rfs); err != nil {
		return nil, fmt.Errorf("%s: rootfolder: %w", c.name, err)
	}
	return rfs, nil
}

// RescanCommand triggers a disk rescan so the instance reconciles its DB with
// what is actually present. command is "RescanSeries" or "RescanMovie".
func (c *Client) Rescan(ctx context.Context) error {
	var name string
	switch c.kind {
	case "sonarr":
		name = "RescanSeries"
	case "radarr":
		name = "RescanMovie"
	default:
		return fmt.Errorf("%s: unknown kind %q", c.name, c.kind)
	}
	body := map[string]string{"name": name}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v3/command", bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("build rescan request: %w", err)
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: rescan: %w", c.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("%s: rescan: status %d: %s", c.name, resp.StatusCode, string(b))
	}
	return nil
}
