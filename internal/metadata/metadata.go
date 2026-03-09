package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Info holds the canonical metadata for a piece of content.
type Info struct {
	Title string
	Year  int
}

// Config holds API keys and feature flags for metadata lookup.
type Config struct {
	TMDBKey string
	OMDBKey string
	TVDBKey string
	Enabled bool
}

// Lookup searches available providers for the best metadata match.
// It tries title variations to improve match rates.
func Lookup(ctx context.Context, title string, year int, isTV bool, cfg Config) (*Info, error) {
	if !cfg.Enabled {
		return &Info{Title: title, Year: year}, nil
	}

	for _, variant := range titleVariants(title) {
		if cfg.TMDBKey != "" {
			if info, err := tmdbSearch(ctx, variant, year, isTV, cfg.TMDBKey); err == nil {
				return info, nil
			}
		}
		if cfg.OMDBKey != "" {
			if info, err := omdbSearch(ctx, variant, year, isTV, cfg.OMDBKey); err == nil {
				return info, nil
			}
		}
		if cfg.TVDBKey != "" && isTV {
			if info, err := tvdbSearch(ctx, variant, cfg.TVDBKey); err == nil {
				return info, nil
			}
		}
	}

	return &Info{Title: title, Year: year}, nil
}

// EpisodeTitle fetches the title of a specific episode from TVDB.
// Returns an empty string (not an error) when TVDB is not configured.
func EpisodeTitle(ctx context.Context, seriesTitle string, season, episode int, cfg Config) (string, error) {
	if cfg.TVDBKey == "" {
		return "", nil
	}

	token, err := tvdbToken(ctx, cfg.TVDBKey)
	if err != nil {
		return "", fmt.Errorf("TVDB auth: %w", err)
	}

	seriesID, err := tvdbSeriesID(ctx, seriesTitle, token)
	if err != nil {
		return "", fmt.Errorf("TVDB series lookup: %w", err)
	}

	return tvdbEpisodeTitle(ctx, seriesID, season, episode, token)
}

// titleVariants generates simplified alternatives to improve metadata match rates.
func titleVariants(title string) []string {
	variants := []string{title}

	if simplified := strings.TrimRight(title, ".,!?:"); simplified != title {
		variants = append(variants, simplified)
	}
	if idx := strings.Index(title, "("); idx > 0 {
		variants = append(variants, strings.TrimSpace(title[:idx]))
	}
	if idx := strings.Index(title, "["); idx > 0 {
		variants = append(variants, strings.TrimSpace(title[:idx]))
	}
	return variants
}

// ---- TMDB ----

func tmdbSearch(ctx context.Context, title string, year int, isTV bool, apiKey string) (*Info, error) {
	endpoint := "movie"
	if isTV {
		endpoint = "tv"
	}

	params := url.Values{"api_key": {apiKey}, "query": {title}}
	if year > 0 {
		if isTV {
			params.Set("first_air_date_year", strconv.Itoa(year))
		} else {
			params.Set("year", strconv.Itoa(year))
		}
	}

	apiURL := fmt.Sprintf("https://api.themoviedb.org/3/search/%s?%s", endpoint, params.Encode())

	var resp struct {
		Results []struct {
			Title        string `json:"title"`
			Name         string `json:"name"`
			ReleaseDate  string `json:"release_date"`
			FirstAirDate string `json:"first_air_date"`
		} `json:"results"`
	}
	if err := getJSON(ctx, apiURL, nil, &resp); err != nil {
		return nil, err
	}
	if len(resp.Results) == 0 {
		return nil, fmt.Errorf("no results")
	}

	r := resp.Results[0]
	info := &Info{}
	if isTV {
		info.Title = r.Name
		if len(r.FirstAirDate) >= 4 {
			info.Year, _ = strconv.Atoi(r.FirstAirDate[:4])
		}
	} else {
		info.Title = r.Title
		if len(r.ReleaseDate) >= 4 {
			info.Year, _ = strconv.Atoi(r.ReleaseDate[:4])
		}
	}
	if info.Title == "" {
		return nil, fmt.Errorf("empty title in response")
	}
	return info, nil
}

// ---- OMDB ----

func omdbSearch(ctx context.Context, title string, year int, isTV bool, apiKey string) (*Info, error) {
	mediaType := "movie"
	if isTV {
		mediaType = "series"
	}

	params := url.Values{"apikey": {apiKey}, "type": {mediaType}, "t": {title}}
	if year > 0 {
		params.Set("y", strconv.Itoa(year))
	}

	var resp struct {
		Response string `json:"Response"`
		Title    string `json:"Title"`
		Year     string `json:"Year"`
	}
	if err := getJSON(ctx, "https://www.omdbapi.com/?"+params.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	if resp.Response != "True" || resp.Title == "" {
		return nil, fmt.Errorf("not found")
	}

	info := &Info{Title: resp.Title}
	if len(resp.Year) >= 4 {
		info.Year, _ = strconv.Atoi(resp.Year[:4])
	}
	return info, nil
}

// ---- TVDB ----

var tvdbCache struct {
	sync.Mutex
	token   string
	expires time.Time
}

func tvdbToken(ctx context.Context, apiKey string) (string, error) {
	tvdbCache.Lock()
	defer tvdbCache.Unlock()

	if tvdbCache.token != "" && time.Now().Before(tvdbCache.expires) {
		return tvdbCache.token, nil
	}

	body := strings.NewReader(fmt.Sprintf(`{"apikey":%q}`, apiKey))
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api4.thetvdb.com/v4/login", body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var authResp struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(data, &authResp); err != nil {
		return "", err
	}
	if authResp.Data.Token == "" {
		return "", fmt.Errorf("empty token from TVDB")
	}

	tvdbCache.token = authResp.Data.Token
	tvdbCache.expires = time.Now().Add(50 * time.Minute)
	return tvdbCache.token, nil
}

func tvdbSearch(ctx context.Context, title, apiKey string) (*Info, error) {
	token, err := tvdbToken(ctx, apiKey)
	if err != nil {
		return nil, err
	}

	apiURL := "https://api4.thetvdb.com/v4/search?query=" + url.QueryEscape(title) + "&type=series"

	var resp struct {
		Data []struct {
			Name       string `json:"name"`
			FirstAired string `json:"first_aired"`
		} `json:"data"`
	}
	if err := getJSON(ctx, apiURL, map[string]string{"Authorization": "Bearer " + token}, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("not found")
	}

	info := &Info{Title: resp.Data[0].Name}
	if len(resp.Data[0].FirstAired) >= 4 {
		info.Year, _ = strconv.Atoi(resp.Data[0].FirstAired[:4])
	}
	return info, nil
}

func tvdbSeriesID(ctx context.Context, title, token string) (int, error) {
	apiURL := "https://api4.thetvdb.com/v4/search?query=" + url.QueryEscape(title) + "&type=series"

	var resp struct {
		Data []struct {
			TVDBId int    `json:"tvdb_id"`
			Name   string `json:"name"`
		} `json:"data"`
	}
	if err := getJSON(ctx, apiURL, map[string]string{"Authorization": "Bearer " + token}, &resp); err != nil {
		return 0, err
	}
	if len(resp.Data) == 0 {
		return 0, fmt.Errorf("series not found: %q", title)
	}
	return resp.Data[0].TVDBId, nil
}

func tvdbEpisodeTitle(ctx context.Context, seriesID, season, episode int, token string) (string, error) {
	apiURL := fmt.Sprintf(
		"https://api4.thetvdb.com/v4/series/%d/episodes/official?season=%d&episodeNumber=%d",
		seriesID, season, episode,
	)

	var resp struct {
		Data struct {
			Episodes []struct {
				Name string `json:"name"`
			} `json:"episodes"`
		} `json:"data"`
	}
	if err := getJSON(ctx, apiURL, map[string]string{"Authorization": "Bearer " + token}, &resp); err != nil {
		return "", err
	}
	if len(resp.Data.Episodes) == 0 {
		return "", fmt.Errorf("episode S%02dE%02d not found", season, episode)
	}
	return resp.Data.Episodes[0].Name, nil
}

// ---- HTTP helper ----

func getJSON(ctx context.Context, rawURL string, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}
