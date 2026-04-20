// Plex Sonic Similar Songs plugin for Navidrome.
//
// Uses Plex's sonic analysis to find similar songs and maps them back to
// Navidrome tracks. Implements the MetadataAgent capability for similar songs.
//
// Build with:
//
//	tinygo build -o plugin.wasm -target wasip1 -buildmode=c-shared .
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/navidrome/navidrome/plugins/pdk/go/host"
	"github.com/navidrome/navidrome/plugins/pdk/go/metadata"
	"github.com/navidrome/navidrome/plugins/pdk/go/pdk"
)

// plexPlugin implements the MetadataAgent similar songs interfaces.
type plexPlugin struct{}

func init() {
	metadata.Register(&plexPlugin{})
}

// Ensure plexPlugin implements the provider interfaces we need.
var (
	_ metadata.SimilarSongsByTrackProvider  = (*plexPlugin)(nil)
	_ metadata.SimilarSongsByArtistProvider = (*plexPlugin)(nil)
)

// --- Plex API Response Types ---

// PlexSearchResult represents the Plex /hubs/search response.
type PlexSearchResult struct {
	MediaContainer struct {
		Hub []PlexHub `json:"Hub"`
	} `json:"MediaContainer"`
}

type PlexHub struct {
	Type     string          `json:"type"`
	Metadata []PlexTrackMeta `json:"Metadata"`
}

// PlexTrackMeta represents a track in Plex search results or sonic similar results.
type PlexTrackMeta struct {
	RatingKey        string `json:"ratingKey"`
	Title            string `json:"title"`
	GrandparentTitle string `json:"grandparentTitle"` // Artist
	ParentTitle      string `json:"parentTitle"`      // Album
	Type             string `json:"type"`
	Duration         int64  `json:"duration"` // milliseconds
}

// PlexSonicResult represents the Plex /library/metadata/{id}/nearest response.
type PlexSonicResult struct {
	MediaContainer struct {
		Metadata []PlexTrackMeta `json:"Metadata"`
	} `json:"MediaContainer"`
}

// --- Configuration Helpers ---

func getPlexURL() string {
	val, ok := pdk.GetConfig("plex_url")
	if !ok || val == "" {
		pdk.Log(pdk.LogWarn, "plex_url not configured")
		return ""
	}
	result := strings.TrimRight(val, "/")
	pdk.Log(pdk.LogDebug, fmt.Sprintf("Config: plex_url=%s", result))
	return result
}

func getPlexToken() string {
	val, ok := pdk.GetConfig("plex_token")
	if !ok || val == "" {
		pdk.Log(pdk.LogWarn, "plex_token not configured")
		return ""
	}
	pdk.Log(pdk.LogDebug, "Config: plex_token=****")
	return val
}

func getMatchThreshold() int {
	val, ok := pdk.GetConfig("match_threshold")
	if !ok || val == "" {
		pdk.Log(pdk.LogDebug, "Config: match_threshold not set, using default 85")
		return 85
	}
	v, err := strconv.Atoi(val)
	if err != nil {
		pdk.Log(pdk.LogWarn, fmt.Sprintf("Config: invalid match_threshold '%s', using default 85", val))
		return 85
	}
	pdk.Log(pdk.LogDebug, fmt.Sprintf("Config: match_threshold=%d", v))
	return v
}

// --- Cache Key Helpers ---

func cacheKey(artist, title string) string {
	h := sha256.Sum256([]byte(normalizeString(artist) + "||" + normalizeString(title)))
	return "similar:" + hex.EncodeToString(h[:16])
}

func cacheKeyByMBID(mbid string) string {
	return "similar:mbid:" + mbid
}

// --- Plex API Communication ---

func plexRequest(endpoint string) ([]byte, error) {
	plexURL := getPlexURL()
	plexToken := getPlexToken()

	if plexURL == "" || plexToken == "" {
		pdk.Log(pdk.LogError, "Cannot make Plex request: plex_url and/or plex_token not configured")
		return nil, fmt.Errorf("plex_url and plex_token must be configured")
	}

	fullURL := plexURL + endpoint
	if strings.Contains(endpoint, "?") {
		fullURL += "&X-Plex-Token=" + url.QueryEscape(plexToken)
	} else {
		fullURL += "?X-Plex-Token=" + url.QueryEscape(plexToken)
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf("Plex HTTP GET: %s", plexURL+endpoint))

	resp, err := host.HTTPSend(host.HTTPRequest{
		Method: "GET",
		URL:    fullURL,
		Headers: map[string]string{
			"Accept":     "application/json",
			"User-Agent": "NavidromePlexSonicPlugin/0.1.0",
		},
		TimeoutMs: 15000,
	})
	if err != nil {
		pdk.Log(pdk.LogError, fmt.Sprintf("Plex HTTP error for %s: %v", endpoint, err))
		return nil, fmt.Errorf("plex HTTP error: %w", err)
	}
	pdk.Log(pdk.LogDebug, fmt.Sprintf("Plex HTTP response: status=%d, bodyLen=%d", resp.StatusCode, len(resp.Body)))
	if resp.StatusCode != 200 {
		pdk.Log(pdk.LogError, fmt.Sprintf("Plex HTTP non-200 for %s: status %d", endpoint, resp.StatusCode))
		return nil, fmt.Errorf("plex HTTP error: status %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// searchPlexTrack searches Plex for a track by artist and title,
// returning the ratingKey of the best match.
func searchPlexTrack(artist, title string) (string, error) {
	pdk.Log(pdk.LogInfo, fmt.Sprintf("Searching Plex for track: '%s - %s'", artist, title))
	query := url.QueryEscape(artist + " " + title)
	endpoint := fmt.Sprintf("/hubs/search?query=%s&limit=5", query)

	body, err := plexRequest(endpoint)
	if err != nil {
		pdk.Log(pdk.LogError, fmt.Sprintf("Plex search request failed for '%s - %s': %v", artist, title, err))
		return "", err
	}

	var result PlexSearchResult
	if err := json.Unmarshal(body, &result); err != nil {
		pdk.Log(pdk.LogError, fmt.Sprintf("Failed to parse Plex search JSON: %v", err))
		return "", fmt.Errorf("failed to parse Plex search response: %w", err)
	}

	hubCount := len(result.MediaContainer.Hub)
	pdk.Log(pdk.LogDebug, fmt.Sprintf("Plex search returned %d hubs", hubCount))

	threshold := getMatchThreshold()

	// Look through hubs for track results
	for _, hub := range result.MediaContainer.Hub {
		if hub.Type != "track" {
			continue
		}
		for _, track := range hub.Metadata {
			score := matchScore(artist, title, track.GrandparentTitle, track.Title)
			pdk.Log(pdk.LogDebug, fmt.Sprintf("Plex match candidate: '%s - %s' score=%d",
				track.GrandparentTitle, track.Title, score))
			if score >= threshold {
				pdk.Log(pdk.LogInfo, fmt.Sprintf("Matched Plex track: ratingKey=%s '%s - %s' (score=%d)",
					track.RatingKey, track.GrandparentTitle, track.Title, score))
				return track.RatingKey, nil
			}
		}
	}

	pdk.Log(pdk.LogWarn, fmt.Sprintf("No matching track found in Plex for '%s - %s' (threshold=%d)", artist, title, threshold))
	return "", fmt.Errorf("no matching track found in Plex for '%s - %s'", artist, title)
}

// getSonicSimilar retrieves sonically similar tracks from Plex.
func getSonicSimilar(ratingKey string, count int) ([]PlexTrackMeta, error) {
	pdk.Log(pdk.LogInfo, fmt.Sprintf("Fetching sonic similar tracks from Plex: ratingKey=%s, count=%d", ratingKey, count))
	endpoint := fmt.Sprintf("/library/metadata/%s/nearest?count=%d", ratingKey, count)

	body, err := plexRequest(endpoint)
	if err != nil {
		pdk.Log(pdk.LogError, fmt.Sprintf("Sonic similar request failed for ratingKey=%s: %v", ratingKey, err))
		return nil, err
	}

	var result PlexSonicResult
	if err := json.Unmarshal(body, &result); err != nil {
		pdk.Log(pdk.LogError, fmt.Sprintf("Failed to parse Plex sonic JSON for ratingKey=%s: %v", ratingKey, err))
		return nil, fmt.Errorf("failed to parse Plex sonic response: %w", err)
	}

	if len(result.MediaContainer.Metadata) == 0 {
		pdk.Log(pdk.LogWarn, fmt.Sprintf("No sonic similar tracks found for ratingKey=%s", ratingKey))
		return nil, fmt.Errorf("no sonic similar tracks found for ratingKey %s", ratingKey)
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf("Found %d sonic similar tracks from Plex for ratingKey=%s", len(result.MediaContainer.Metadata), ratingKey))
	for i, t := range result.MediaContainer.Metadata {
		pdk.Log(pdk.LogDebug, fmt.Sprintf("  Sonic result [%d]: '%s - %s' (album: '%s')", i+1, t.GrandparentTitle, t.Title, t.ParentTitle))
	}
	return result.MediaContainer.Metadata, nil
}

// --- String Matching & Normalization ---

// normalizeString prepares a string for comparison by lowercasing and removing
// common noise words like "Remastered", "Deluxe Edition", etc.
func normalizeString(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))

	// Remove common suffixes/noise
	noisePatterns := []string{
		"(remastered)",
		"(remaster)",
		"[remastered]",
		"[remaster]",
		"- remastered",
		"- remaster",
		"(deluxe edition)",
		"(deluxe)",
		"[deluxe edition]",
		"[deluxe]",
		"(bonus track version)",
		"(bonus tracks)",
		"(expanded edition)",
		"(special edition)",
		"(anniversary edition)",
		"(live)",
		"[live]",
		"(single version)",
		"(mono)",
		"(stereo)",
		"(radio edit)",
	}

	for _, noise := range noisePatterns {
		s = strings.ReplaceAll(s, noise, "")
	}

	// Remove trailing " - " artifacts
	s = strings.TrimRight(s, " -")
	s = strings.TrimSpace(s)

	// Collapse multiple spaces
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}

	return s
}

// matchScore returns a similarity score (0-100) between two artist+title pairs.
func matchScore(artist1, title1, artist2, title2 string) int {
	a1 := normalizeString(artist1)
	t1 := normalizeString(title1)
	a2 := normalizeString(artist2)
	t2 := normalizeString(title2)

	artistScore := stringSimilarity(a1, a2)
	titleScore := stringSimilarity(t1, t2)

	// Weight title match slightly higher since artist names may have variations
	return (artistScore*40 + titleScore*60) / 100
}

// stringSimilarity returns a similarity score (0-100) between two strings
// using a simple character bigram approach.
func stringSimilarity(a, b string) int {
	if a == b {
		return 100
	}
	if len(a) == 0 || len(b) == 0 {
		return 0
	}

	// Check if one contains the other
	if strings.Contains(a, b) || strings.Contains(b, a) {
		shorter := len(a)
		longer := len(b)
		if shorter > longer {
			shorter, longer = longer, shorter
		}
		return (shorter * 100) / longer
	}

	// Bigram similarity (Dice coefficient)
	bigramsA := makeBigrams(a)
	bigramsB := makeBigrams(b)

	if len(bigramsA) == 0 || len(bigramsB) == 0 {
		return 0
	}

	intersect := 0
	for bg := range bigramsA {
		if bigramsB[bg] > 0 {
			min := bigramsA[bg]
			if bigramsB[bg] < min {
				min = bigramsB[bg]
			}
			intersect += min
		}
	}

	return (2 * intersect * 100) / (len(a) - 1 + len(b) - 1)
}

func makeBigrams(s string) map[string]int {
	bigrams := make(map[string]int)
	runes := []rune(s)
	for i := 0; i < len(runes)-1; i++ {
		bg := string(runes[i : i+2])
		bigrams[bg]++
	}
	return bigrams
}

// --- Plex Results to Navidrome SongRef Conversion ---

func plexTracksToSongRefs(tracks []PlexTrackMeta) []metadata.SongRef {
	refs := make([]metadata.SongRef, 0, len(tracks))
	for _, t := range tracks {
		ref := metadata.SongRef{
			Name:   t.Title,
			Artist: t.GrandparentTitle,
			Album:  t.ParentTitle,
		}
		if t.Duration > 0 {
			ref.Duration = float32(t.Duration) / 1000.0
		}
		refs = append(refs, ref)
	}
	return refs
}

// --- KVStore Cache Helpers ---

func getCachedSimilar(key string) (*metadata.SimilarSongsResponse, bool) {
	pdk.Log(pdk.LogDebug, fmt.Sprintf("Checking cache for key: %s", key))
	data, exists, err := host.KVStoreGet(key)
	if err != nil {
		pdk.Log(pdk.LogWarn, fmt.Sprintf("KVStore read error for key %s: %v", key, err))
		return nil, false
	}
	if !exists {
		pdk.Log(pdk.LogDebug, fmt.Sprintf("Cache miss for key: %s", key))
		return nil, false
	}

	var resp metadata.SimilarSongsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		pdk.Log(pdk.LogDebug, fmt.Sprintf("Cache unmarshal error for key %s: %v", key, err))
		return nil, false
	}

	pdk.Log(pdk.LogDebug, fmt.Sprintf("Cache hit for key %s (%d songs)", key, len(resp.Songs)))
	return &resp, true
}

func setCachedSimilar(key string, resp *metadata.SimilarSongsResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		pdk.Log(pdk.LogError, fmt.Sprintf("Cache marshal error: %v", err))
		return
	}

	pdk.Log(pdk.LogDebug, fmt.Sprintf("Caching %d songs to key %s (TTL=7d, size=%d bytes)", len(resp.Songs), key, len(data)))
	// Store with 7-day TTL
	if err := host.KVStoreSetWithTTL(key, data, 7*24*60*60); err != nil {
		pdk.Log(pdk.LogError, fmt.Sprintf("KVStore write error for key %s: %v", key, err))
	} else {
		pdk.Log(pdk.LogInfo, fmt.Sprintf("Cached %d similar songs to key %s", len(resp.Songs), key))
	}
}

// --- MetadataAgent Capability Implementations ---

// GetSimilarSongsByTrack is the main entry point called by Navidrome when a user
// requests "Instant Mix" or similar songs for a specific track.
func (p *plexPlugin) GetSimilarSongsByTrack(req metadata.SimilarSongsByTrackRequest) (*metadata.SimilarSongsResponse, error) {
	pdk.Log(pdk.LogInfo, fmt.Sprintf("GetSimilarSongsByTrack: name='%s', artist='%s', mbid='%s', count=%d",
		req.Name, req.Artist, req.MBID, req.Count))

	// Step 1: Check cache
	var kvKey string
	if req.MBID != "" {
		kvKey = cacheKeyByMBID(req.MBID)
	} else {
		kvKey = cacheKey(req.Artist, req.Name)
	}

	if cached, ok := getCachedSimilar(kvKey); ok {
		pdk.Log(pdk.LogInfo, fmt.Sprintf("Returning %d cached similar songs for '%s - %s'", len(cached.Songs), req.Artist, req.Name))
		// Respect the requested count
		if int(req.Count) > 0 && len(cached.Songs) > int(req.Count) {
			cached.Songs = cached.Songs[:req.Count]
		}
		return cached, nil
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf("Cache miss, querying Plex for '%s - %s'", req.Artist, req.Name))

	// Step 2: Forward search — find the track in Plex
	ratingKey, err := searchPlexTrack(req.Artist, req.Name)
	if err != nil {
		pdk.Log(pdk.LogInfo, fmt.Sprintf("Plex search failed: %v", err))
		return nil, fmt.Errorf("plex search failed: %w", err)
	}

	// Step 3: Get sonic similar tracks from Plex
	count := int(req.Count)
	if count <= 0 {
		count = 20
	}
	// Request extra from Plex in case some don't match back
	plexTracks, err := getSonicSimilar(ratingKey, count*2)
	if err != nil {
		pdk.Log(pdk.LogInfo, fmt.Sprintf("Plex sonic similar failed: %v", err))
		return nil, fmt.Errorf("plex sonic retrieval failed: %w", err)
	}

	// Step 4: Convert Plex results to SongRef (Navidrome will reconcile by name/artist)
	pdk.Log(pdk.LogDebug, fmt.Sprintf("Converting %d Plex tracks to SongRefs", len(plexTracks)))
	songRefs := plexTracksToSongRefs(plexTracks)

	resp := &metadata.SimilarSongsResponse{
		Songs: songRefs,
	}

	// Step 5: Cache the full result set
	setCachedSimilar(kvKey, resp)

	// Trim to requested count
	if count > 0 && len(resp.Songs) > count {
		resp.Songs = resp.Songs[:count]
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf("Returning %d similar songs for '%s - %s'",
		len(resp.Songs), req.Artist, req.Name))
	return resp, nil
}

// GetSimilarSongsByArtist returns similar songs for an artist by searching
// for the artist in Plex and finding sonic neighbours of their top tracks.
func (p *plexPlugin) GetSimilarSongsByArtist(req metadata.SimilarSongsByArtistRequest) (*metadata.SimilarSongsResponse, error) {
	pdk.Log(pdk.LogInfo, fmt.Sprintf("GetSimilarSongsByArtist: name='%s', mbid='%s', count=%d",
		req.Name, req.MBID, req.Count))

	// Search for the artist in Plex to find their tracks
	pdk.Log(pdk.LogInfo, fmt.Sprintf("Searching Plex for artist: '%s'", req.Name))
	query := url.QueryEscape(req.Name)
	endpoint := fmt.Sprintf("/hubs/search?query=%s&limit=3", query)

	body, err := plexRequest(endpoint)
	if err != nil {
		return nil, fmt.Errorf("plex artist search failed: %w", err)
	}

	var result PlexSearchResult
	if err := json.Unmarshal(body, &result); err != nil {
		pdk.Log(pdk.LogError, fmt.Sprintf("Failed to parse Plex artist search JSON: %v", err))
		return nil, fmt.Errorf("failed to parse Plex search response: %w", err)
	}

	// Find the first track by this artist
	pdk.Log(pdk.LogDebug, fmt.Sprintf("Searching %d hubs for tracks by artist '%s'", len(result.MediaContainer.Hub), req.Name))
	var firstTrackKey string
	for _, hub := range result.MediaContainer.Hub {
		if hub.Type != "track" {
			continue
		}
		for _, track := range hub.Metadata {
			score := stringSimilarity(normalizeString(req.Name), normalizeString(track.GrandparentTitle))
			pdk.Log(pdk.LogDebug, fmt.Sprintf("Artist match candidate: '%s' score=%d", track.GrandparentTitle, score))
			if score >= getMatchThreshold() {
				pdk.Log(pdk.LogInfo, fmt.Sprintf("Matched artist track: ratingKey=%s '%s - %s' (score=%d)", track.RatingKey, track.GrandparentTitle, track.Title, score))
				firstTrackKey = track.RatingKey
				break
			}
		}
		if firstTrackKey != "" {
			break
		}
	}

	if firstTrackKey == "" {
		pdk.Log(pdk.LogWarn, fmt.Sprintf("No matching tracks found in Plex for artist '%s'", req.Name))
		return nil, fmt.Errorf("no tracks found in Plex for artist '%s'", req.Name)
	}

	count := int(req.Count)
	if count <= 0 {
		count = 20
	}

	plexTracks, err := getSonicSimilar(firstTrackKey, count*2)
	if err != nil {
		return nil, fmt.Errorf("plex sonic retrieval failed: %w", err)
	}

	songRefs := plexTracksToSongRefs(plexTracks)
	if len(songRefs) > count {
		songRefs = songRefs[:count]
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf("Returning %d similar songs for artist '%s'", len(songRefs), req.Name))
	return &metadata.SimilarSongsResponse{Songs: songRefs}, nil
}

func main() {}
